package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/yourGaomu/short_links/back/internal/modules/templates"
)

func registerTemplateRoutes(router *gin.Engine, deps Dependencies) {
	// 1. 模板查询和维护使用不同权限；管理员权限由 RequirePermission 统一放行。
	service := deps.Templates
	api := router.Group("/api/v1")
	api.Use(authenticate(deps.Auth))
	api.POST("/rule-templates", requirePermission(deps.DB, "templates.manage"), func(c *gin.Context) {
		var input templates.CreateRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.Create(c.Request.Context(), templateActor(c), input, templateMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	api.GET("/rule-templates", requirePermission(deps.DB, "templates.read_use"), func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		includeDeleted := false
		if raw := c.Query("includeDeleted"); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				writeError(c, http.StatusUnprocessableEntity, "invalid_filter", "includeDeleted 参数无效")
				return
			}
			includeDeleted = parsed
		}
		result, err := service.List(c.Request.Context(), templateActor(c), page, pageSize, c.Query("workspaceId"), c.Query("status"), c.Query("keyword"), includeDeleted)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.GET("/rule-templates/:templateId/versions/:version", requirePermission(deps.DB, "templates.read_use"), func(c *gin.Context) {
		result, err := service.Version(c.Request.Context(), templateActor(c), c.Param("templateId"), c.Param("version"))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.GET("/rule-templates/:templateId/versions", requirePermission(deps.DB, "templates.read_use"), func(c *gin.Context) {
		result, err := service.Versions(c.Request.Context(), templateActor(c), c.Param("templateId"))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/rule-templates/:templateId/versions", requirePermission(deps.DB, "templates.manage"), func(c *gin.Context) {
		var input templates.VersionRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.CreateVersion(c.Request.Context(), templateActor(c), c.Param("templateId"), input, templateMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	api.GET("/rule-templates/:templateId", requirePermission(deps.DB, "templates.read_use"), func(c *gin.Context) {
		result, err := service.Get(c.Request.Context(), templateActor(c), c.Param("templateId"))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.PATCH("/rule-templates/:templateId", requirePermission(deps.DB, "templates.manage"), func(c *gin.Context) {
		var input templates.UpdateRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := service.Update(c.Request.Context(), templateActor(c), c.Param("templateId"), input, templateMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/rule-templates/:templateId/disable", requirePermission(deps.DB, "templates.manage"), templateStateHandler(service, "disable"))
	api.POST("/rule-templates/:templateId/enable", requirePermission(deps.DB, "templates.manage"), templateStateHandler(service, "enable"))
	api.DELETE("/rule-templates/:templateId", requirePermission(deps.DB, "templates.manage"), templateStateHandler(service, "delete"))
	api.POST("/rule-templates/:templateId/restore", requirePermission(deps.DB, "templates.manage"), templateStateHandler(service, "restore"))
}

func templateStateHandler(service *templates.Service, action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := service.SetState(c.Request.Context(), templateActor(c), c.Param("templateId"), action, templateMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func templateActor(c *gin.Context) templates.Actor {
	identity := currentAuth(c)
	workspaceID, _ := parsePublicID("ws", identity.User.DefaultWorkspaceID)
	return templates.Actor{UserID: identity.User.ID, WorkspaceID: workspaceID, Role: identity.User.Role}
}

func templateMeta(c *gin.Context) templates.Meta {
	return templates.Meta{SourceIP: c.ClientIP(), UserAgent: c.Request.UserAgent(), RequestID: requestID(c), AuditDetails: apiKeyAuditDetails(c)}
}
