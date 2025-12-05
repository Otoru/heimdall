package server

import (
	"context"
	"time"

	"go.uber.org/zap"
)

func RunChecksumScanner(ctx context.Context, logger *zap.Logger, store Storage, prefix string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	running := make(chan struct{}, 1)

	logger.Info("checksum scanner started", zap.Duration("interval", interval), zap.String("prefix", prefix))

	for {
		select {
		case running <- struct{}{}:
			go func() {
				defer func() { <-running }()
				if err := store.CleanupBadChecksums(ctx, prefix); err != nil {
					logger.Warn("checksum cleanup failed", zap.Error(err))
				}
				if err := store.GenerateChecksums(ctx, prefix); err != nil {
					logger.Warn("checksum scan failed", zap.Error(err))
				}
			}()
		default:
			logger.Warn("checksum scan skipped; previous run still in progress")
		}

		select {
		case <-ctx.Done():
			logger.Info("checksum scanner stopped")
			return
		case <-ticker.C:
		}
	}
}
