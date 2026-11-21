package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yourGaomu/short_links/back/internal/modules/notifications"
)

func registerNotificationRoutes(router *gin.Engine, deps Dependencies) {
	service := notifications.NewService(deps.DB)
	api := router.Group("/api/v1")
	api.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
	}, authenticate(deps.Auth), requireHumanSession())

	api.GET("/notifications", func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		unreadOnly := false
		if values, exists := c.Request.URL.Query()["unread"]; exists {
			if len(values) != 1 || (values[0] != "true" && values[0] != "false") {
				writeError(c, http.StatusUnprocessableEntity, "invalid_unread_filter", "未读筛选参数无效")
				return
			}
			unreadOnly = values[0] == "true"
		}
		result, err := service.List(c.Request.Context(), currentAuth(c).User.ID, unreadOnly, page, pageSize)
		if err != nil {
			writeNotificationServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.GET("/notifications/unread-count", func(c *gin.Context) {
		count, err := service.UnreadCount(c.Request.Context(), currentAuth(c).User.ID)
		if err != nil {
			writeNotificationServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"unreadCount": count})
	})
	api.PATCH("/notifications/:id/read", func(c *gin.Context) {
		if err := service.MarkRead(c.Request.Context(), currentAuth(c).User.ID, c.Param("id")); err != nil {
			writeNotificationServiceError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	api.POST("/notifications/read-all", func(c *gin.Context) {
		count, err := service.ReadAll(c.Request.Context(), currentAuth(c).User.ID)
		if err != nil {
			writeNotificationServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"markedCount": count})
	})
	api.GET("/me/notification-settings", func(c *gin.Context) {
		settings, err := service.GetSettings(c.Request.Context(), currentAuth(c).User.ID)
		if err != nil {
			writeNotificationServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, settings)
	})
	api.PATCH("/me/notification-settings", func(c *gin.Context) {
		var input notifications.UpdateSettingsRequest
		if !decodeJSON(c, &input) {
			return
		}
		settings, err := service.UpdateSettings(c.Request.Context(), currentAuth(c).User.ID, input)
		if err != nil {
			writeNotificationServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, settings)
	})
}

func writeNotificationServiceError(c *gin.Context, err error) {
	if problem, ok := err.(*notifications.Problem); ok {
		writeError(c, problem.Status, problem.Code, problem.Message)
		return
	}
	writeServiceError(c, err)
}
