package server

import (
	"context"
	"time"

	"go.uber.org/zap"
)

func RunChecksumScanner(ctx context.Context, logger *zap.Logger, store Storage, prefix string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Info("checksum scanner started", zap.Duration("interval", interval), zap.String("prefix", prefix))

	for {
		if err := store.GenerateChecksums(ctx, prefix); err != nil {
			logger.Warn("checksum scan failed", zap.Error(err))
		}

		select {
		case <-ctx.Done():
			logger.Info("checksum scanner stopped")
			return
		case <-ticker.C:
		}
	}
}
