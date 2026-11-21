package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/yourGaomu/short_links/back/internal/modules/links"
)

func registerLinkGroupRoutes(router *gin.Engine, deps Dependencies) {
	api := router.Group("/api/v1")
	api.Use(authenticate(deps.Auth), requireHumanSession())
	api.GET("/link-groups", requirePermission(deps.DB, "links.read"), func(c *gin.Context) {
		page, pageSize, ok := parsePagination(c)
		if !ok {
			return
		}
		query := map[string]string{}
		if c.Request.URL.Query().Has("workspaceId") {
			query["workspaceId"] = c.Query("workspaceId")
		}
		result, err := deps.Links.ListGroups(c.Request.Context(), linkActor(c), page, pageSize, query)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/link-groups", requirePermission(deps.DB, "links.manage"), func(c *gin.Context) {
		var input links.CreateGroupRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := deps.Links.CreateGroup(c.Request.Context(), linkActor(c), input, linkMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	api.PATCH("/link-groups/:groupId", requirePermission(deps.DB, "links.manage"), func(c *gin.Context) {
		var input links.UpdateGroupRequest
		if !decodeJSON(c, &input) {
			return
		}
		result, err := deps.Links.UpdateGroup(c.Request.Context(), linkActor(c), c.Param("groupId"), input, linkMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.DELETE("/link-groups/:groupId", requirePermission(deps.DB, "links.manage"), func(c *gin.Context) {
		result, err := deps.Links.DeleteGroup(c.Request.Context(), linkActor(c), c.Param("groupId"), linkMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
}
