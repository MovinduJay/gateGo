package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/example/gatego/internal/buildinfo"
	"github.com/example/gatego/internal/config"
	"github.com/example/gatego/internal/gateway"
	gmetrics "github.com/example/gatego/internal/metrics"
	"github.com/example/gatego/internal/observability"
)

func main() {
	configPath := flag.String("config", "configs/gatego.json", "path to gateway JSON config")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	if port := os.Getenv("PORT"); port != "" {
		cfg.Server.Listen = ":" + port
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := observability.InitTracing(ctx, cfg.Observability.ServiceName, logger)
	if err != nil {
		logger.Error("failed to initialize OpenTelemetry", "error", err)
		os.Exit(1)
	}

	jwtSecret := os.Getenv("GATEGO_JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "dev-only-change-me"
		logger.Warn("using development JWT secret")
	}
	adminKey := os.Getenv("GATEGO_ADMIN_KEY")
	if adminKey == "" {
		adminKey = "dev-admin-key"
		logger.Warn("using development admin key")
	}

	m := gmetrics.New()
	redisLimiter := gateway.RedisFromEnv(ctx, logger)
	gw, err := gateway.New(*configPath, cfg, jwtSecret, adminKey, redisLimiter, m, logger)
	if err != nil {
		logger.Error("failed to initialize gateway", "error", err)
		os.Exit(1)
	}
	defer gw.Close()
	go gw.HealthChecker().Run(ctx)
	go gw.WatchConfig(ctx, cfg.Server.ConfigReloadInterval.Std())

	mux := http.NewServeMux()
	mux.Handle("/metrics", m)
	mux.Handle("/version", buildinfo.Handler())
	mux.Handle("/", gw)
	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           observability.AccessLog(observability.NewHTTPHandler(mux), logger),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		MaxHeaderBytes:    cfg.Server.MaxRequestHeaderBytes,
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := gw.Reload(); err != nil {
				logger.Error("SIGHUP reload failed", "error", err)
			}
		}
	}()
	go func() {
		logger.Info("GateGo listening", "address", cfg.Server.Listen)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Std())
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	if err := shutdownTracing(shutdownCtx); err != nil {
		logger.Error("OpenTelemetry shutdown failed", "error", err)
	}
	fmt.Println("GateGo stopped")
}
