package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yourGaomu/short_links/back/internal/modules/assets"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
	"github.com/yourGaomu/short_links/back/internal/modules/business"
	"github.com/yourGaomu/short_links/back/internal/modules/links"
	"github.com/yourGaomu/short_links/back/internal/modules/logs"
	"github.com/yourGaomu/short_links/back/internal/modules/templates"
)

type authContext struct {
	User      auth.User
	SessionID string
	APIKey    *auth.APIKeyIdentity
}

func registerAuthRoutes(router *gin.Engine, service *auth.Service, db *sql.DB) {
	// 1. 注册公开认证路由；这些路由不接受客户端自报用户 ID 或角色。
	api := router.Group("/api/v1")
	api.POST("/auth/email-codes", func(c *gin.Context) {
		var input auth.EmailCodeRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.RequestEmailCode(c.Request.Context(), input, c.ClientIP())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, result)
	})
	api.POST("/auth/register", func(c *gin.Context) {
		var input auth.RegisterRequest
		if !decodeJSON(c, &input) {
			return
		}
		user, err := service.Register(c.Request.Context(), input, authMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"user":      user,
			"workspace": gin.H{"workspaceId": user.DefaultWorkspaceID, "name": "默认空间"},
		})
	})
	api.POST("/auth/login", func(c *gin.Context) {
		var input auth.LoginRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.Login(c.Request.Context(), input, c.GetHeader("X-Client-App"), authMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/auth/password-resets", func(c *gin.Context) {
		var input auth.PasswordResetRequest
		if !decodeJSON(c, &input) {
			return
		}
		if err := service.PasswordReset(c.Request.Context(), input, authMeta(c)); err != nil {
			writeServiceError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	// 2. 受保护路由统一在进入 Handler 前解析 Bearer Token 并校验数据库会话。
	protected := api.Group("")
	protected.Use(authenticate(service))
	protected.GET("/me", requireAccountAccess(), func(c *gin.Context) {
		identity := currentAuth(c)
		c.JSON(http.StatusOK, identity.User)
	})
	protected.POST("/auth/logout", requireHumanSession(), func(c *gin.Context) {
		if err := service.Logout(c.Request.Context(), currentAuth(c).User.ID, currentAuth(c).SessionID, authMeta(c)); err != nil {
			writeServiceError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	protected.GET("/me/permissions", requireAccountAccess(), func(c *gin.Context) {
		identity := currentAuth(c)
		result, err := service.MyPermissions(c.Request.Context(), identity.User.ID, identity.User.Role)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	protected.GET("/me/usage", requireAccountAccess(), func(c *gin.Context) {
		identity := currentAuth(c)
		quotas, err := service.UserQuotas(c.Request.Context(), identity.User.ID)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		storage := gin.H{"maxSingleFileBytes": quotas.MaxSingleFileBytes, "quotaBytes": quotas.StorageQuotaBytes, "usedBytes": quotas.StorageUsedBytes, "remainingBytes": max(int64(0), quotas.StorageQuotaBytes-quotas.StorageUsedBytes)}
		var urlLimit, assetLimit any = quotas.URLLinkQuota, quotas.MinioAssetLinkQuota
		var urlRemaining, assetRemaining any = max(0, *quotas.URLLinkQuota-int(quotas.URLLinkUsed)), max(0, *quotas.MinioAssetLinkQuota-int(quotas.MinioAssetLinkUsed))
		if identity.User.Role == "admin" {
			urlLimit, assetLimit, urlRemaining, assetRemaining = nil, nil, nil, nil
		}
		apiKeyLimit := any(quotas.APIKeyQuota)
		apiKeyRemaining := any(max(0, quotas.APIKeyQuota-int(quotas.APIKeyUsed)))
		if identity.User.Role == "admin" {
			apiKeyLimit, apiKeyRemaining = nil, nil
		}
		c.JSON(http.StatusOK, gin.H{"storage": storage, "apiKeys": gin.H{"used": quotas.APIKeyUsed, "limit": apiKeyLimit, "remaining": apiKeyRemaining}, "urlLinks": gin.H{"used": quotas.URLLinkUsed, "limit": urlLimit, "remaining": urlRemaining}, "minioAssetLinks": gin.H{"used": quotas.MinioAssetLinkUsed, "limit": assetLimit, "remaining": assetRemaining}})
	})
	protected.GET("/invites", requireHumanSession(), requirePermission(db, "invites.create_revoke"), func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		result, err := service.ListInvites(c.Request.Context(), currentAuth(c).User, c.Query("status"), c.Query("createdByUserId"), page, pageSize)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	protected.POST("/invites", requireHumanSession(), requirePermission(db, "invites.create_revoke"), func(c *gin.Context) {
		var input auth.InviteCreateRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.CreateInvite(c.Request.Context(), currentAuth(c).User, input, authMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	protected.POST("/invites/:inviteId/revoke", requireHumanSession(), requirePermission(db, "invites.create_revoke"), func(c *gin.Context) {
		result, err := service.RevokeInvite(c.Request.Context(), currentAuth(c).User, c.Param("inviteId"), authMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin := protected.Group("/admin")
	admin.Use(requireHumanSession(), requireAdmin())
	registerAdminRoutes(admin, service)
	protected.GET("/me/sessions", requireHumanSession(), func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		identity := currentAuth(c)
		items, total, err := service.ListSessions(c.Request.Context(), identity.User.ID, identity.SessionID, page, pageSize)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"items":      items,
			"pagination": gin.H{"page": page, "pageSize": pageSize, "total": total},
		})
	})
	protected.POST("/me/sessions/heartbeat", requireHumanSession(), func(c *gin.Context) {
		session, err := service.Heartbeat(c.Request.Context(), currentAuth(c).User.ID, currentAuth(c).SessionID)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, session)
	})
	protected.DELETE("/me/sessions/:sessionId", requireHumanSession(), func(c *gin.Context) {
		sessionID, err := parsePublicID("ses", c.Param("sessionId"))
		if err != nil {
			writeError(c, http.StatusNotFound, "session_not_found", "会话不存在")
			return
		}
		if err := service.RevokeSession(c.Request.Context(), currentAuth(c).User.ID, currentAuth(c).User.ID, sessionID.String(), authMeta(c)); err != nil {
			writeServiceError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}

func authMeta(c *gin.Context) auth.AuditMeta {
	return auth.AuditMeta{SourceIP: c.ClientIP(), UserAgent: truncateLogValue(c.Request.UserAgent(), 1024), RequestID: requestID(c)}
}

func authenticate(service *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 机器凭证支持 X-API-Key 或 Bearer stub_sk_；两者同时出现时必须一致。
		apiKeyHeader := strings.TrimSpace(c.GetHeader("X-API-Key"))
		header := strings.TrimSpace(c.GetHeader("Authorization"))
		bearer := ""
		if strings.HasPrefix(strings.ToLower(header), "bearer ") {
			bearer = strings.TrimSpace(header[len("Bearer "):])
		}
		if apiKeyHeader != "" || strings.HasPrefix(bearer, "stub_sk_") {
			if apiKeyHeader != "" && bearer != "" && apiKeyHeader != bearer {
				writeError(c, http.StatusUnauthorized, "invalid_api_key", "API Key 无效")
				return
			}
			secret := apiKeyHeader
			if secret == "" {
				secret = bearer
			}
			identity, err := service.AuthenticateAPIKey(c.Request.Context(), secret, c.ClientIP())
			if err != nil {
				writeServiceError(c, err)
				return
			}
			c.Set("authContext", authContext{User: identity.User, APIKey: &identity})
			c.Next()
			return
		}
		// 2. 人类会话必须使用 Bearer Session Token。
		if bearer == "" {
			writeError(c, http.StatusUnauthorized, "unauthorized", "请先登录")
			return
		}
		user, sessionID, err := service.Authenticate(c.Request.Context(), bearer)
		if err != nil {
			writeServiceError(c, err)
			return
		}

		// 2. 将数据库确认的身份放入请求上下文，后续权限中间件和 Handler 只信任此值。
		c.Set("authContext", authContext{User: user, SessionID: sessionID})
		c.Next()
	}
}

func requireHumanSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if currentAuth(c).APIKey != nil {
			writeError(c, http.StatusForbidden, "api_key_forbidden_endpoint", "该接口不支持 API Key 调用")
			return
		}
		c.Next()
	}
}

func requireAccountAccess() gin.HandlerFunc {
	return func(c *gin.Context) {
		identity := currentAuth(c)
		if identity.APIKey != nil && !identity.APIKey.AllowAccountAccess {
			writeError(c, http.StatusForbidden, "api_key_account_access_denied", "该 API Key 未开启账号信息访问能力")
			return
		}
		c.Next()
	}
}

func currentAuth(c *gin.Context) authContext {
	value, _ := c.Get("authContext")
	identity, _ := value.(authContext)
	return identity
}

func apiKeyBusinessID(c *gin.Context) string {
	identity := currentAuth(c)
	if identity.APIKey == nil {
		return ""
	}
	return identity.APIKey.BusinessID
}

func apiKeyID(c *gin.Context) *uuid.UUID {
	identity := currentAuth(c)
	if identity.APIKey == nil {
		return nil
	}
	id := identity.APIKey.ID
	return &id
}

func apiKeyAuditDetails(c *gin.Context) map[string]any {
	identity := currentAuth(c)
	if identity.APIKey == nil {
		return nil
	}
	return map[string]any{"authType": "api_key", "apiKeyId": identity.APIKey.PublicID, "apiKeyPrefix": identity.APIKey.Prefix, "apiKeyName": identity.APIKey.Name}
}

func decodeJSON(c *gin.Context, destination any) bool {
	// 1. 限制 JSON 请求体大小并拒绝未知字段，防止拼错参数被静默忽略。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(c, http.StatusUnprocessableEntity, "invalid_request", "请求参数无效")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(c, http.StatusUnprocessableEntity, "invalid_request", "请求参数无效")
		return false
	}
	return true
}

func parsePagination(c *gin.Context) (int, int, bool) {
	page, pageSize := 1, 20
	var err error
	if raw := c.Query("page"); raw != "" {
		page, err = strconv.Atoi(raw)
	}
	if raw := c.Query("pageSize"); err == nil && raw != "" {
		pageSize, err = strconv.Atoi(raw)
	}
	if err != nil || page < 1 || pageSize < 1 || pageSize > 100 {
		writeError(c, http.StatusUnprocessableEntity, "invalid_pagination", "分页参数无效")
		return 0, 0, false
	}
	return page, pageSize, true
}

func parsePublicID(prefix, value string) (uuid.UUID, error) {
	// 1. 公共 ID 以固定前缀区分资源类型，数据库仍使用 UUID 主键。
	trimmed, ok := strings.CutPrefix(value, prefix+"_")
	if !ok {
		return uuid.Nil, fmt.Errorf("invalid %s id", prefix)
	}
	return uuid.Parse(trimmed)
}

func writeServiceError(c *gin.Context, err error) {
	// 1. 领域问题转换为稳定的 HTTP 契约；内部异常只记录请求 ID。
	var problem *auth.Problem
	if errors.As(err, &problem) {
		writeError(c, problem.Status, problem.Code, problem.Message)
		return
	}
	var linkProblem *links.Problem
	if errors.As(err, &linkProblem) {
		writeError(c, linkProblem.Status, linkProblem.Code, linkProblem.Message)
		return
	}
	var templateProblem *templates.Problem
	if errors.As(err, &templateProblem) {
		writeError(c, templateProblem.Status, templateProblem.Code, templateProblem.Message)
		return
	}
	var businessProblem *business.Problem
	if errors.As(err, &businessProblem) {
		writeError(c, businessProblem.Status, businessProblem.Code, businessProblem.Message)
		return
	}
	var assetProblem *assets.Problem
	if errors.As(err, &assetProblem) {
		writeError(c, assetProblem.Status, assetProblem.Code, assetProblem.Message)
		return
	}
	var logProblem *logs.Problem
	if errors.As(err, &logProblem) {
		writeError(c, logProblem.Status, logProblem.Code, logProblem.Message)
		return
	}
	if logger, ok := c.Get("logger"); ok {
		if value, ok := logger.(*slog.Logger); ok {
			value.Error("request_failed", "request_id", requestID(c), "error_type", fmt.Sprintf("%T", err))
		}
	}
	writeError(c, http.StatusInternalServerError, "internal_error", "服务内部错误")
}
