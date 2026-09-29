package server

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// RunChecksumScanner periodically backfills missing checksums and removes
// chained ones left behind by older versions.
//
// lease, when non-nil, gates each tick so that only one replica scans. Every
// replica used to run this loop, which under an autoscaler meant N full-bucket
// listings plus a GET of every object missing a checksum, N times per interval.
// The work is idempotent, so the lease is an optimisation rather than a
// correctness requirement; passing nil keeps the old always-scan behaviour.
func RunChecksumScanner(ctx context.Context, logger *zap.Logger, store Storage, prefix string, interval time.Duration, lease *Lease) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	running := make(chan struct{}, 1)

	logger.Info("checksum scanner started",
		zap.Duration("interval", interval),
		zap.String("prefix", prefix),
		zap.Bool("leader_election", lease != nil),
	)

	for {
		select {
		case running <- struct{}{}:
			go func() {
				defer func() { <-running }()

				if lease != nil {
					ok, err := lease.TryAcquire(ctx)
					if err != nil {
						logger.Warn("checksum scan skipped; lease unavailable", zap.Error(err))
						return
					}
					if !ok {
						logger.Debug("checksum scan skipped; another replica holds the lease")
						return
					}
				}

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
