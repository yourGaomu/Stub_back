package httpapi

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/yourGaomu/short_links/back/internal/config"
	"github.com/yourGaomu/short_links/back/internal/modules/assets"
	"github.com/yourGaomu/short_links/back/internal/modules/auth"
	"github.com/yourGaomu/short_links/back/internal/modules/business"
	"github.com/yourGaomu/short_links/back/internal/modules/links"
	"github.com/yourGaomu/short_links/back/internal/modules/logs"
	"github.com/yourGaomu/short_links/back/internal/modules/templates"
	"github.com/yourGaomu/short_links/back/internal/platform/objectstore"
)

type Dependencies struct {
	Config      config.Config
	Logger      *slog.Logger
	DB          *sql.DB
	DBPing      func(context.Context) error
	Redis       *redis.Client
	Auth        *auth.Service
	Assets      *assets.Service
	Links       *links.Service
	Templates   *templates.Service
	Businesses  *business.Service
	Logs        *logs.Service
	ObjectStore *objectstore.Store
}

func NewRouter(deps Dependencies) (*gin.Engine, error) {
	// 1. 在注册路由前验证组合根依赖，防止运行时因漏注入而空指针崩溃。
	if deps.Logger == nil || deps.DB == nil || deps.DBPing == nil || deps.Redis == nil || deps.Auth == nil || deps.Assets == nil || deps.Links == nil || deps.Templates == nil || deps.Businesses == nil || deps.Logs == nil {
		return nil, fmt.Errorf("HTTP 路由依赖未完整注入")
	}
	// 2. 仅启用显式注册的中间件，避免 Gin 默认调试输出泄露部署信息。
	if deps.Config.Environment == "production" {
		gin.SetMode(gin.ReleaseMode)
	}
	router := gin.New()
	if err := router.SetTrustedProxies(deps.Config.TrustedProxyCIDRs); err != nil {
		return nil, err
	}
	router.Use(gin.Recovery(), requestContext(deps.Logger))
	router.Use(cors.New(cors.Config{
		AllowOrigins:     append(deps.Config.CORSAllowedOrigins, "https://localhost:5174", "http://localhost"),
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Authorization", "Content-Type", "X-API-Key", "X-Client-App", "X-Request-ID"},
		ExposeHeaders:    []string{"X-Request-ID"},
		AllowCredentials: true,
	}))

	// 3. 区分进程存活和依赖就绪，供容器探针分别使用。
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", func(c *gin.Context) {
		if err := deps.DBPing(c.Request.Context()); err != nil {
			writeError(c, http.StatusServiceUnavailable, "database_unavailable", "服务暂不可用")
			return
		}
		if err := deps.Redis.Ping(c.Request.Context()).Err(); err != nil {
			writeError(c, http.StatusServiceUnavailable, "cache_unavailable", "服务暂不可用")
			return
		}
		if deps.ObjectStore != nil {
			if err := deps.ObjectStore.Ping(c.Request.Context()); err != nil {
				writeError(c, http.StatusServiceUnavailable, "object_store_unavailable", "服务暂不可用")
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	// 4. 按业务域注册路由；领域服务由 app 组合根构造并注入。
	registerAuthRoutes(router, deps.Auth, deps.DB)
	registerAPIKeyRoutes(router, deps.Auth, deps.DB)
	registerLinkRoutes(router, deps)
	registerTemplateRoutes(router, deps)
	registerBusinessRoutes(router, deps)
	registerAssetRoutes(router, deps)
	registerLogRoutes(router, deps)
	return router, nil
}
