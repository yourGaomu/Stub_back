package httpapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func requirePermission(db *sql.DB, permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. API Key 必须同时具备 Key scope；用户权限仍从数据库实时读取，二者取交集。
		identity := currentAuth(c)
		if identity.APIKey != nil && !identity.APIKey.Scopes[permission] {
			writeError(c, http.StatusForbidden, "api_key_scope_denied", "API Key 没有执行此操作的权限")
			return
		}
		if identity.User.Role == "admin" {
			c.Next()
			return
		}
		var enabled bool
		err := db.QueryRowContext(c.Request.Context(), `SELECT EXISTS (SELECT 1 FROM user_permissions WHERE user_id=$1 AND permission_code=$2 AND granted_at IS NOT NULL AND revoked_at IS NULL)`, identity.User.ID, permission).Scan(&enabled)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "permission_check_failed", "服务内部错误")
			return
		}
		if !enabled {
			writeError(c, http.StatusForbidden, "permission_denied", "当前账号没有执行此操作的权限")
			return
		}
		c.Next()
	}
}

func requireAPIKeyBusinessAccess(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := currentAuth(c)
		if identity.APIKey == nil {
			c.Next()
			return
		}
		var allowed bool
		err := db.QueryRowContext(c.Request.Context(), `SELECT EXISTS(SELECT 1 FROM business_integrations bi WHERE bi.business_id=$1 AND bi.status='active' AND ($2='admin' OR EXISTS(SELECT 1 FROM user_business_access uba WHERE uba.user_id=$3 AND uba.business_id=$1 AND uba.revoked_at IS NULL)))`, identity.APIKey.BusinessID, identity.User.Role, identity.User.ID).Scan(&allowed)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "business_access_check_failed", "服务内部错误")
			return
		}
		if !allowed {
			writeError(c, http.StatusForbidden, "api_key_business_denied", "API Key 绑定的业务已停用或不再授权")
			return
		}
		c.Next()
	}
}

func requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 仅使用认证中间件从数据库确认的角色，不接收客户端自报角色。
		if currentAuth(c).APIKey != nil {
			writeError(c, http.StatusForbidden, "api_key_forbidden_endpoint", "管理员治理接口必须使用人类登录会话")
			return
		}
		if currentAuth(c).User.Role != "admin" {
			writeError(c, http.StatusForbidden, "admin_required", "需要管理员权限")
			return
		}
		c.Next()
	}
}

func requireCreateLinkPermission(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 只读取目标类型来选择权限，随后完整恢复请求体供 DTO Handler 解析。
		identity := currentAuth(c)
		if identity.APIKey != nil && !identity.APIKey.Scopes["links.create_url"] && !identity.APIKey.Scopes["links.create_asset"] {
			writeError(c, http.StatusForbidden, "api_key_scope_denied", "API Key 没有创建短链的权限")
			return
		}
		if identity.User.Role == "admin" && identity.APIKey == nil {
			c.Next()
			return
		}
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, (1<<20)+1))
		if err != nil || len(body) > 1<<20 {
			writeError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体过大")
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		var shape struct {
			Target struct {
				Type string `json:"type"`
			} `json:"target"`
			RuleSource struct {
				Type string `json:"type"`
			} `json:"ruleSource"`
		}
		if err := json.Unmarshal(body, &shape); err != nil {
			c.Next()
			return
		}
		permission := "links.create_url"
		if strings.TrimSpace(shape.Target.Type) == "minio_asset" {
			permission = "links.create_asset"
		}
		permissions := []string{permission}
		if shape.RuleSource.Type == "template" {
			permissions = append(permissions, "templates.read_use")
		}
		for _, required := range permissions {
			if identity.APIKey != nil && !identity.APIKey.Scopes[required] {
				writeError(c, http.StatusForbidden, "api_key_scope_denied", "API Key 没有执行此操作的权限")
				return
			}
			if identity.APIKey != nil && identity.User.Role == "admin" {
				continue
			}
			var enabled bool
			if err := db.QueryRowContext(c.Request.Context(), `SELECT EXISTS (SELECT 1 FROM user_permissions WHERE user_id=$1 AND permission_code=$2 AND granted_at IS NOT NULL AND revoked_at IS NULL)`, currentAuth(c).User.ID, required).Scan(&enabled); err != nil {
				writeError(c, http.StatusInternalServerError, "permission_check_failed", "服务内部错误")
				return
			}
			if !enabled {
				writeError(c, http.StatusForbidden, "permission_denied", "当前账号没有执行此操作的权限")
				return
			}
		}
		c.Next()
	}
}

func requireMultipartBusinessAccess(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 先按账号单文件额度限制 multipart 请求，再读取表单中的业务标识。
		identity := currentAuth(c)
		var maxSingle int64
		if err := db.QueryRowContext(c.Request.Context(), `SELECT max_single_file_bytes FROM users WHERE user_id=$1`, identity.User.ID).Scan(&maxSingle); err != nil {
			writeError(c, http.StatusInternalServerError, "quota_check_failed", "服务内部错误")
			return
		}
		limit := int64(math.MaxInt64)
		if maxSingle <= math.MaxInt64-(2<<20) {
			limit = maxSingle + (2 << 20)
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		if err := c.Request.ParseMultipartForm(8 << 20); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(c, http.StatusRequestEntityTooLarge, "upload_quota_exceeded", "上传内容超过允许大小")
				return
			}
			writeError(c, http.StatusUnprocessableEntity, "invalid_upload", "上传表单无效")
			return
		}
		if c.Request.MultipartForm != nil {
			defer c.Request.MultipartForm.RemoveAll()
		}
		businessID := strings.TrimSpace(c.Request.FormValue("businessId"))
		if keyBusiness := apiKeyBusinessID(c); keyBusiness != "" {
			if businessID != "" && businessID != keyBusiness {
				writeError(c, http.StatusForbidden, "api_key_business_denied", "API Key 未授权该业务")
				return
			}
			businessID = keyBusiness
			if c.Request.MultipartForm != nil {
				c.Request.MultipartForm.Value["businessId"] = []string{keyBusiness}
			}
		}
		if businessID == "" {
			writeError(c, http.StatusNotFound, "business_not_found", "业务不存在或无权访问")
			return
		}
		var allowed bool
		err := db.QueryRowContext(c.Request.Context(), `SELECT EXISTS(SELECT 1 FROM business_integrations bi WHERE bi.business_id=$2 AND bi.status='active' AND ($3='admin' OR EXISTS(SELECT 1 FROM user_business_access uba WHERE uba.user_id=$1 AND uba.business_id=bi.business_id AND uba.revoked_at IS NULL)))`, identity.User.ID, businessID, identity.User.Role).Scan(&allowed)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "business_access_check_failed", "服务内部错误")
			return
		}
		if !allowed {
			writeError(c, http.StatusNotFound, "business_not_found", "业务不存在或无权访问")
			return
		}
		c.Next()
	}
}
