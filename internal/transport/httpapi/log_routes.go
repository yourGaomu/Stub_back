package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/yourGaomu/short_links/back/internal/modules/logs"
)

func registerLogRoutes(router *gin.Engine, deps Dependencies) {
	// 1. 日志与统计共用身份认证；普通用户的功能权限在业务 Handler 前校验。
	service := deps.Logs
	api := router.Group("/api/v1")
	api.Use(authenticate(deps.Auth))
	api.GET("/logs/access", requirePermission(deps.DB, "logs.read_own"), func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		ungrouped := false
		if raw, present := c.GetQuery("ungrouped"); present {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeError(c, http.StatusUnprocessableEntity, "invalid_filter", "ungrouped 参数无效")
				return
			}
			ungrouped = value
		}
		result, err := service.Access(c.Request.Context(), logActor(c), logs.AccessFilter{
			GroupID: c.Query("groupId"), Ungrouped: ungrouped,
			LinkID: c.Query("linkId"), From: c.Query("from"), To: c.Query("to"), Result: c.Query("result"), Reason: c.Query("reason"),
			WorkspaceID: c.Query("workspaceId"), BusinessID: c.Query("businessId"), LinkOwnerUserID: c.Query("linkOwnerUserId"), Page: page, PageSize: pageSize,
		})
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.GET("/logs/audit", requirePermission(deps.DB, "logs.read_own"), func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		result, err := service.Audit(c.Request.Context(), logActor(c), logs.AuditFilter{
			WorkspaceID: c.Query("workspaceId"), ActorUserID: c.Query("actorUserId"), BusinessID: c.Query("businessId"),
			ResourceType: c.Query("resourceType"), ResourceID: c.Query("resourceId"), Action: c.Query("action"), Result: c.Query("result"),
			From: c.Query("from"), To: c.Query("to"), Page: page, PageSize: pageSize,
		})
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.GET("/analytics/summary", requirePermission(deps.DB, "links.read"), func(c *gin.Context) {
		result, err := service.Summary(c.Request.Context(), logActor(c), c.Query("range"), c.Query("timeZone"), c.Query("workspaceId"))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.GET("/analytics/links/:linkId", requirePermission(deps.DB, "links.read"), func(c *gin.Context) {
		result, err := service.Link(c.Request.Context(), logActor(c), c.Param("linkId"), c.Query("range"), c.Query("timeZone"))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.GET("/admin/security-events/unknown-short-codes", requireAdmin(), func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		filter := logs.UnknownFilter{From: c.Query("from"), To: c.Query("to"), SourceIP: c.Query("sourceIp"), Page: page, PageSize: pageSize}
		if raw := c.Query("rateLimited"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeError(c, http.StatusUnprocessableEntity, "invalid_filter", "rateLimited 参数无效")
				return
			}
			filter.RateLimited = &value
		}
		result, err := service.UnknownShortCodes(c.Request.Context(), logActor(c), filter, logsMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
}

func logActor(c *gin.Context) logs.Actor {
	identity := currentAuth(c)
	workspaceID, _ := parsePublicID("ws", identity.User.DefaultWorkspaceID)
	return logs.Actor{UserID: identity.User.ID, WorkspaceID: workspaceID, Email: identity.User.Email, Role: identity.User.Role}
}

func logsMeta(c *gin.Context) logs.Meta {
	return logs.Meta{SourceIP: c.ClientIP(), UserAgent: truncateLogValue(c.Request.UserAgent(), 1024), RequestID: requestID(c)}
}
