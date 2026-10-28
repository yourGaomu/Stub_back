package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/yourGaomu/short_links/back/internal/modules/assets"
)

func registerAssetRoutes(router *gin.Engine, deps Dependencies) {
	// 1. 素材管理使用用户功能权限；上传另校验 multipart 中业务标识的授权。
	service := deps.Assets
	api := router.Group("/api/v1")
	api.Use(authenticate(deps.Auth))
	api.Use(requireAPIKeyBusinessAccess(deps.DB))
	api.GET("/assets/uploads", requirePermission(deps.DB, "assets.upload"), func(c *gin.Context) {
		result, err := service.ListMultipartUploads(c.Request.Context(), assetActor(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": result})
	})
	api.POST("/assets/uploads", requirePermission(deps.DB, "assets.upload"), func(c *gin.Context) {
		var input struct {
			BusinessID     string `json:"businessId"`
			Path           string `json:"path"`
			FileName       string `json:"fileName"`
			ContentType    string `json:"contentType"`
			SizeBytes      int64  `json:"sizeBytes"`
			IdempotencyKey string `json:"idempotencyKey"`
		}
		if err := c.ShouldBindJSON(&input); err != nil {
			writeError(c, http.StatusUnprocessableEntity, "invalid_upload", "上传会话参数无效")
			return
		}
		if keyBusiness := apiKeyBusinessID(c); keyBusiness != "" {
			if input.BusinessID != "" && input.BusinessID != keyBusiness {
				writeError(c, http.StatusForbidden, "api_key_business_denied", "API Key 未授权该业务")
				return
			}
			input.BusinessID = keyBusiness
		}
		result, err := service.BeginMultipartUpload(c.Request.Context(), assetActor(c), assets.BeginUploadInput{BusinessID: input.BusinessID, Path: input.Path, FileName: input.FileName, ContentType: input.ContentType, SizeBytes: input.SizeBytes, IdempotencyKey: input.IdempotencyKey})
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	api.PUT("/assets/uploads/:uploadSessionId/parts/:partNumber", requirePermission(deps.DB, "assets.upload"), func(c *gin.Context) {
		partNumber, err := strconv.Atoi(c.Param("partNumber"))
		if err != nil {
			writeError(c, http.StatusUnprocessableEntity, "upload_part_invalid", "分片序号无效")
			return
		}
		const maxPartSize = int64(32 * 1024 * 1024)
		if c.Request.ContentLength > maxPartSize || c.Request.ContentLength < 0 {
			writeError(c, http.StatusRequestEntityTooLarge, "upload_part_invalid", "分片请求必须提供不超过 32 MiB 的 Content-Length")
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPartSize)
		result, err := service.UploadMultipartPart(c.Request.Context(), assetActor(c), assets.UploadPartInput{SessionID: c.Param("uploadSessionId"), PartNumber: partNumber, Reader: c.Request.Body, ContentLength: c.Request.ContentLength})
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.GET("/assets/uploads/:uploadSessionId", requirePermission(deps.DB, "assets.upload"), func(c *gin.Context) {
		result, err := service.GetMultipartUpload(c.Request.Context(), assetActor(c), c.Param("uploadSessionId"))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/assets/uploads/:uploadSessionId/complete", requirePermission(deps.DB, "assets.upload"), func(c *gin.Context) {
		result, err := service.CompleteMultipartUpload(c.Request.Context(), assetActor(c), c.Param("uploadSessionId"), assetMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.DELETE("/assets/uploads/:uploadSessionId", requirePermission(deps.DB, "assets.upload"), func(c *gin.Context) {
		result, err := service.CancelMultipartUpload(c.Request.Context(), assetActor(c), c.Param("uploadSessionId"))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/assets", requirePermission(deps.DB, "assets.upload"), requireMultipartBusinessAccess(deps.DB), func(c *gin.Context) {
		form := c.Request.MultipartForm
		if form == nil || len(form.File["file"]) != 1 {
			writeError(c, http.StatusUnprocessableEntity, "invalid_upload", "必须上传一个文件")
			return
		}
		businessID := c.Request.FormValue("businessId")
		if keyBusiness := apiKeyBusinessID(c); keyBusiness != "" {
			if businessID != "" && businessID != keyBusiness {
				writeError(c, http.StatusForbidden, "api_key_business_denied", "API Key 未授权该业务")
				return
			}
			businessID = keyBusiness
		}
		result, err := service.Upload(c.Request.Context(), assetActor(c), assets.UploadInput{BusinessID: businessID, Path: c.Request.FormValue("path"), File: form.File["file"][0]}, assetMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	api.GET("/assets", requirePermission(deps.DB, "assets.read"), func(c *gin.Context) {
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
		businessID := c.Query("businessId")
		if keyBusiness := apiKeyBusinessID(c); keyBusiness != "" {
			if businessID != "" && businessID != keyBusiness {
				writeError(c, http.StatusForbidden, "api_key_business_denied", "API Key 未授权该业务")
				return
			}
			businessID = keyBusiness
		}
		result, err := service.List(c.Request.Context(), assetActor(c), page, pageSize, businessID, c.Query("workspaceId"), c.Query("ownerUserId"), c.Query("keyword"), includeDeleted)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.GET("/assets/:assetId/content", requirePermission(deps.DB, "assets.read"), func(c *gin.Context) {
		location, err := service.Content(c.Request.Context(), assetActor(c), c.Param("assetId"), assetMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.Redirect(http.StatusFound, location)
	})
	api.GET("/assets/:assetId", requirePermission(deps.DB, "assets.read"), func(c *gin.Context) {
		result, err := service.Get(c.Request.Context(), assetActor(c), c.Param("assetId"))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.DELETE("/assets/:assetId", requirePermission(deps.DB, "assets.delete"), func(c *gin.Context) {
		result, err := service.Delete(c.Request.Context(), assetActor(c), c.Param("assetId"), assetMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	api.POST("/assets/:assetId/restore", requirePermission(deps.DB, "assets.restore"), func(c *gin.Context) {
		result, err := service.Restore(c.Request.Context(), assetActor(c), c.Param("assetId"), assetMeta(c))
		if err != nil {
			writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
}

func assetActor(c *gin.Context) assets.Actor {
	identity := currentAuth(c)
	workspace, _ := parsePublicID("ws", identity.User.DefaultWorkspaceID)
	return assets.Actor{UserID: identity.User.ID, WorkspaceID: workspace, Role: identity.User.Role, BusinessID: apiKeyBusinessID(c), APIKeyID: apiKeyID(c)}
}

func assetMeta(c *gin.Context) assets.Meta {
	return assets.Meta{SourceIP: c.ClientIP(), UserAgent: truncateLogValue(c.Request.UserAgent(), 1024), RequestID: requestID(c), AuditDetails: apiKeyAuditDetails(c)}
}
