package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

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
	"github.com/yourGaomu/short_links/back/internal/platform/postgres"
	"github.com/yourGaomu/short_links/back/internal/platform/redisclient"
	"github.com/yourGaomu/short_links/back/internal/transport/httpapi"
)

type App struct {
	db          *sql.DB
	redis       *redis.Client
	objectStore *objectstore.Store
	logger      *slog.Logger
	router      *gin.Engine
	cleanerStop context.CancelFunc
	cleanerWG   sync.WaitGroup
}

func New(ctx context.Context, cfg config.Config) (*App, error) {
	// 1. 初始化结构化日志，后续组件统一复用同一个 Logger。
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// 2. 连接 PostgreSQL 并应用嵌入式迁移。
	db, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	// 3. 连接 Redis；该依赖仅供短期状态、缓存和限流使用。
	redisClient, err := redisclient.Open(ctx, cfg.RedisURL)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	// 4. 初始化 MinIO 双 endpoint 客户端；未配置时允许纯 URL 短链服务启动。
	store, err := objectstore.New(cfg)
	if err != nil {
		_ = redisClient.Close()
		_ = db.Close()
		return nil, err
	}

	// 5. 引导首位管理员；已有管理员时不会读取部署密码覆盖账号。
	if err := ensureInitialAdmin(ctx, db, cfg); err != nil {
		if store != nil {
			store.Close()
		}
		_ = redisClient.Close()
		_ = db.Close()
		return nil, fmt.Errorf("管理员初始化失败: %w", err)
	}

	// 6. 在应用组合根创建领域服务；HTTP 层只负责解析 DTO 和返回协议响应。
	application := &App{db: db, redis: redisClient, objectStore: store, logger: logger}
	authService := auth.NewService(db, redisClient, cfg)
	assetService := assets.NewService(db, store)
	linkService := links.NewService(db, cfg, store)
	templateService := templates.NewService(db)
	businessService := business.NewService(db, store)
	logService := logs.NewService(db)

	// 7. 将领域服务和基础设施依赖注入 HTTP 路由，避免路由层自行组装业务对象。
	router, err := httpapi.NewRouter(httpapi.Dependencies{
		Config:      cfg,
		Logger:      logger,
		DB:          db,
		DBPing:      db.PingContext,
		Redis:       redisClient,
		Auth:        authService,
		Assets:      assetService,
		Links:       linkService,
		Templates:   templateService,
		Businesses:  businessService,
		Logs:        logService,
		ObjectStore: store,
	})
	if err != nil {
		if store != nil {
			store.Close()
		}
		_ = redisClient.Close()
		_ = db.Close()
		return nil, fmt.Errorf("创建 HTTP 路由失败: %w", err)
	}
	application.router = router
	application.startAssetCleaner(assetService)
	return application, nil
}

func (a *App) Router() *gin.Engine { return a.router }

func (a *App) Close() {
	// 1. 先停止后台任务并等待其释放数据库/对象存储连接。
	if a.cleanerStop != nil {
		a.cleanerStop()
		a.cleanerWG.Wait()
	}
	// 2. 按依赖生命周期逆序释放连接。
	if a.objectStore != nil {
		a.objectStore.Close()
	}
	if a.redis != nil {
		_ = a.redis.Close()
	}
	if a.db != nil {
		_ = a.db.Close()
	}
}

func (a *App) startAssetCleaner(service *assets.Service) {
	// 1. 单实例 worker 定期清理到期素材；多实例时由数据库 SKIP LOCKED 分配工作。
	ctx, cancel := context.WithCancel(context.Background())
	a.cleanerStop = cancel
	a.cleanerWG.Add(1)
	go func() {
		defer a.cleanerWG.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			workCtx, workCancel := context.WithTimeout(ctx, 2*time.Minute)
			if err := service.PurgeExpired(workCtx, 20); err != nil && ctx.Err() == nil {
				a.logger.Error("asset_purge_failed", "error", err)
			}
			if err := service.CleanupMultipartUploads(workCtx, 20); err != nil && ctx.Err() == nil {
				a.logger.Error("asset_upload_cleanup_failed", "error", err)
			}
			workCancel()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
