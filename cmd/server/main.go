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

	"github.com/joho/godotenv"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/app"
	"github.com/lkmaVanilla/tripo-3d-agent/internal/web"
)

func main() {
	if err := web.CheckBuild(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		slog.Error("读取 .env 失败")
		os.Exit(1)
	}
	cfg, err := app.ConfigFromEnv()
	if err != nil {
		slog.Error("配置无效", "error", err)
		os.Exit(1)
	}
	service, err := app.New(cfg)
	if err != nil {
		slog.Error("初始化服务失败", "error", err)
		os.Exit(1)
	}
	service.Start()
	server := &http.Server{Addr: cfg.Listen, Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		slog.Info("Tripo Agent 已启动", "address", cfg.Listen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP 服务失败", "error", err)
			cancel()
		}
	}()
	<-ctx.Done()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	if err := service.Close(); err != nil {
		slog.Error("关闭服务失败", "error", err)
	}
}
