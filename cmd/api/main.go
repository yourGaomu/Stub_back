package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourGaomu/short_links/back/internal/app"
	"github.com/yourGaomu/short_links/back/internal/config"
	"github.com/yourGaomu/short_links/back/internal/platform/envfile"
)

func main() { os.Exit(run()) }

func run() int {
	// 1. 读取并校验运行配置，避免服务带着不完整配置启动。
	if err := envfile.LoadForDevelopment(); err != nil {
		slog.Error("加载环境文件失败", "error", err)
		return 1
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Error("配置校验失败", "error", err)
		return 1
	}

	// 2. 创建服务依赖并完成迁移、基础数据初始化。
	application, err := app.New(context.Background(), cfg)
	if err != nil {
		slog.Error("服务初始化失败", "error", err)
		return 1
	}
	defer application.Close()

	// 3. 启动 HTTP 服务，并在进程退出时等待正在处理的请求完成。
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.Router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Minute,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("HTTP 服务启动", "addr", cfg.HTTPAddr, "env", cfg.Environment)
		serverErrors <- server.ListenAndServe()
	}()

	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP 服务异常退出", "error", err)
			return 1
		}
	case <-stop.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("HTTP 服务优雅退出失败", "error", err)
		}
	}
	return 0
}
