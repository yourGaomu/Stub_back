package httpapi

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
)

func registerAPIKeyRoutes(router *gin.Engine, service *auth.Service, db *sql.DB) {
	// 1. API Key 管理只能由人类会话执行，机器 Key 不能创建或修改另一把 Key。
	api := router.Group("/api/v1")
	api.Use(authenticate(service))
	api.GET("/api-keys", requireHumanSession(), requirePermission(db, "api_keys.manage"), func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		result, err := service.ListAPIKeys(c.Request.Context(), currentAuth(c).User.ID, c.Query("status"), page, pageSize)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/api-keys", requireHumanSession(), requirePermission(db, "api_keys.manage"), func(c *gin.Context) {
		var input auth.CreateAPIKeyRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.CreateAPIKey(c.Request.Context(), currentAuth(c).User, input, authMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	api.PATCH("/api-keys/:apiKeyId", requireHumanSession(), requirePermission(db, "api_keys.manage"), func(c *gin.Context) {
		var input auth.UpdateAPIKeyRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.UpdateAPIKey(c.Request.Context(), currentAuth(c).User, c.Param("apiKeyId"), input, authMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	for _, item := range []struct{ path, status string }{{"disable", "disabled"}, {"enable", "active"}, {"revoke", "revoked"}} {
		item := item
		api.POST("/api-keys/:apiKeyId/"+item.path, requireHumanSession(), requirePermission(db, "api_keys.manage"), func(c *gin.Context) {
			result, err := service.SetAPIKeyStatus(c.Request.Context(), currentAuth(c).User, c.Param("apiKeyId"), item.status, authMeta(c))
			if err != nil {
				writeServiceError(c, err)
				return
			}
			c.JSON(http.StatusOK, result)
		})
	}

	admin := api.Group("/admin")
	admin.Use(requireHumanSession(), requireAdmin())
	admin.GET("/users/:userId/api-keys", func(c *gin.Context) {
		userID, err := parsePublicID("usr", c.Param("userId"))
		if err != nil {
			writeError(c, http.StatusNotFound, "user_not_found", "用户不存在")
			return
		}
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		result, err := service.AdminListAPIKeys(c.Request.Context(), userID, c.Query("status"), page, pageSize)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		if !recordAdminRead(c, service, "admin.api_keys.read", "user", userID.String(), map[string]any{"status": c.Query("status"), "page": page, "pageSize": pageSize}) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin.POST("/api-keys/:apiKeyId/revoke", func(c *gin.Context) {
		if err := service.AdminRevokeAPIKey(c.Request.Context(), currentAuth(c).User.ID, c.Param("apiKeyId"), authMeta(c)); err != nil {
			writeServiceError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}
