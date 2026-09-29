package main

//go:generate swag init -g main.go -o ../internal/docs

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/otoru/heimdall/internal/config"
	"github.com/otoru/heimdall/internal/docs"
	"github.com/otoru/heimdall/internal/metrics"
	"github.com/otoru/heimdall/internal/server"
	"github.com/otoru/heimdall/internal/storage"
	"go.uber.org/zap"
)

// @title Heimdall API
// @version 1.0
// @description Maven-compatible HTTP server backed by S3-compatible storage.
// @BasePath /
// @securityDefinitions.basic BasicAuth
// @securityDefinitions.apikey ApiKeyAuth
// @in header
// @name X-API-Key
func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}

	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer func() { _ = logger.Sync() }()

	ctx := context.Background()
	store, err := storage.New(ctx, storage.Options{
		Bucket:       cfg.Bucket,
		Prefix:       cfg.Prefix,
		Region:       cfg.Region,
		Endpoint:     cfg.Endpoint,
		AccessKey:    cfg.AccessKey,
		SecretKey:    cfg.SecretKey,
		UsePathStyle: cfg.UsePathStyle,
	})
	if err != nil {
		logger.Fatal("init storage", zap.Error(err))
	}

	appMetrics := metrics.New()
	docs.SwaggerInfo.BasePath = "/"
	docs.SwaggerInfo.Title = "Heimdall API"
	docs.SwaggerInfo.Version = "1.0"

	proxyOpts := server.ProxyOptions{
		CacheTTL:        cfg.ProxyCacheTTL,
		CacheStaleGrace: cfg.ProxyCacheStaleGrace,
		RetryAttempts:   cfg.UpstreamRetryAttempts,
		VerifyChecksums: cfg.VerifyUpstreamChecksums,
		Timeout:         cfg.UpstreamTimeout,
		OnCacheResult: func(r server.ProxyCacheResult) {
			appMetrics.ProxyConfigCache.WithLabelValues(string(r)).Inc()
		},
		OnRetry: appMetrics.UpstreamRetries.Inc,
		OnFetch: func(proxy, result string) {
			appMetrics.ProxyFetch.WithLabelValues(proxy, result).Inc()
		},
	}

	srv := server.NewWithProxyOptions(store, logger, appMetrics, cfg.AuthUser, cfg.AuthPassword, cfg.APIKeyEndpoint, cfg.APIKeyToken, proxyOpts)

	httpServer := &http.Server{
		Addr:    cfg.Addr,
		Handler: srv.Handler(),
	}
	metricsServer := &http.Server{
		Addr:    cfg.MetricsAddr,
		Handler: metrics.HandlerFor(appMetrics),
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_ = metricsServer.Shutdown(ctx)
		if err := httpServer.Shutdown(ctx); err != nil {
			logger.Error("shutdown error", zap.Error(err))
		}
		close(idleConnsClosed)
	}()

	go func() {
		logger.Info("metrics server starting", zap.String("addr", cfg.MetricsAddr))
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("metrics server failed", zap.Error(err))
		}
	}()

	ctx, cancelScan := context.WithCancel(context.Background())
	defer cancelScan()
	intervalStr := cfg.ChecksumScanInterval
	if intervalStr == "" {
		intervalStr = "30m"
	}
	dur, err := time.ParseDuration(intervalStr)
	if err != nil {
		logger.Warn("invalid CHECKSUM_SCAN_INTERVAL, skipping scanner", zap.Error(err))
	} else if dur > 0 {
		// Under an autoscaler every replica would otherwise run a full-bucket
		// scan each interval. The lease keeps that to one replica; the scan is
		// idempotent, so a brief split brain only wastes work.
		var lease *server.Lease
		if cfg.ScannerLeaderElection {
			lease = server.NewLease(store, logger, 2*dur)
		}
		go server.RunChecksumScanner(ctx, logger, store, cfg.ChecksumScanPrefix, dur, lease)
	}

	logger.Info("server starting", zap.String("addr", cfg.Addr), zap.String("bucket", cfg.Bucket), zap.String("prefix", cfg.Prefix))

	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatal("serve", zap.Error(err))
	}

	<-idleConnsClosed
}
