package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/yourGaomu/short_links/back/internal/modules/business"
)

func registerBusinessRoutes(router *gin.Engine, deps Dependencies) {
	// 1. 业务标识只在需要业务上下文的接口使用，不能充当用户身份或权限。
	service := deps.Businesses
	api := router.Group("/api/v1")
	api.Use(authenticate(deps.Auth))
	api.GET("/me/businesses", func(c *gin.Context) {
		identity := currentAuth(c)
		if identity.APIKey != nil && !identity.APIKey.AllowAccountAccess {
			writeError(c, http.StatusForbidden, "api_key_account_access_denied", "该 API Key 未开启账号信息访问能力")
			return
		}
		items, err := service.Me(c.Request.Context(), business.Actor{UserID: identity.User.ID, Role: identity.User.Role, BusinessID: apiKeyBusinessID(c)})
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})
	admin := api.Group("/admin")
	admin.Use(requireAdmin())
	admin.GET("/businesses", func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		result, err := service.List(c.Request.Context(), page, pageSize, c.Query("status"))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin.POST("/businesses", func(c *gin.Context) {
		var input business.CreateRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.Create(c.Request.Context(), business.Actor{UserID: currentAuth(c).User.ID, Role: "admin"}, input, businessMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	admin.PATCH("/businesses/:businessId", func(c *gin.Context) {
		var input business.UpdateRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.Update(c.Request.Context(), business.Actor{UserID: currentAuth(c).User.ID, Role: "admin"}, c.Param("businessId"), input, businessMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin.GET("/users/:userId/businesses", func(c *gin.Context) {
		userID, ok := businessUserID(c)
		if !ok {
			return
		}
		result, err := service.UserAccess(c.Request.Context(), userID)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin.PUT("/users/:userId/businesses/:businessId", func(c *gin.Context) {
		userID, ok := businessUserID(c)
		if !ok {
			return
		}
		result, err := service.Grant(c.Request.Context(), business.Actor{UserID: currentAuth(c).User.ID, Role: "admin"}, userID, c.Param("businessId"), businessMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin.DELETE("/users/:userId/businesses/:businessId", func(c *gin.Context) {
		userID, ok := businessUserID(c)
		if !ok {
			return
		}
		if err := service.Revoke(c.Request.Context(), business.Actor{UserID: currentAuth(c).User.ID, Role: "admin"}, userID, c.Param("businessId"), businessMeta(c)); err != nil {
			writeServiceError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}

func businessUserID(c *gin.Context) (uuid.UUID, bool) {
	id, err := parsePublicID("usr", c.Param("userId"))
	if err != nil {
		writeError(c, http.StatusNotFound, "user_not_found", "用户不存在")
		return uuid.Nil, false
	}
	return id, true
}

func businessMeta(c *gin.Context) business.Meta {
	return business.Meta{SourceIP: c.ClientIP(), UserAgent: truncateLogValue(c.Request.UserAgent(), 1024), RequestID: requestID(c), AuditDetails: apiKeyAuditDetails(c)}
}
