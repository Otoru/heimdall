package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/otoru/heimdall/internal/storage"
	"go.uber.org/zap"
)

// leaseKey is where the scanner lease lives. It shares the control-object
// namespace with the proxy definitions and is never served as an artifact.
const leaseKey = "__heimdall__/scanner.lease"

// leaseConfirmDelay is how long an acquirer waits before reading its own write
// back. Two pods that decide to acquire at the same instant will both write;
// the read-back is what makes at most one of them proceed.
var leaseConfirmDelay = 750 * time.Millisecond

type leaseRecord struct {
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Lease is a best-effort, storage-backed lease used to keep replicas from
// duplicating a periodic background job.
//
// It is NOT mutual exclusion. The object store offers no compare-and-set, so
// two replicas can briefly both believe they hold it. That is acceptable here
// precisely because the work it guards is idempotent: the worst outcome of a
// split brain is duplicated scanning, never a corrupted bucket. Do not reuse
// this for anything where two holders would be unsafe.
type Lease struct {
	store  Storage
	logger *zap.Logger
	holder string
	ttl    time.Duration
	now    func() time.Time
}

func NewLease(store Storage, logger *zap.Logger, ttl time.Duration) *Lease {
	return &Lease{
		store:  store,
		logger: logger,
		holder: leaseHolderID(),
		ttl:    ttl,
		now:    time.Now,
	}
}

func leaseHolderID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

func (l *Lease) read(ctx context.Context) (leaseRecord, bool, error) {
	resp, err := l.store.Get(ctx, leaseKey)
	if err != nil {
		if storage.IsNotFound(err) {
			return leaseRecord{}, false, nil
		}
		return leaseRecord{}, false, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return leaseRecord{}, false, err
	}
	var rec leaseRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		// A corrupt lease is treated as no lease; it will be overwritten.
		return leaseRecord{}, false, nil
	}
	return rec, true, nil
}

func (l *Lease) write(ctx context.Context) error {
	rec := leaseRecord{Holder: l.holder, ExpiresAt: l.now().Add(l.ttl)}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return l.store.Put(ctx, leaseKey, strings.NewReader(string(data)), "application/json", int64(len(data)))
}

// TryAcquire reports whether this process should run the guarded job now.
//
// It returns false, with no error, when another replica holds an unexpired
// lease. Errors reading or writing the lease are returned so the caller can
// decide; the scanner treats them as "skip this tick".
func (l *Lease) TryAcquire(ctx context.Context) (bool, error) {
	rec, exists, err := l.read(ctx)
	if err != nil {
		return false, err
	}

	if exists && rec.Holder != l.holder && l.now().Before(rec.ExpiresAt) {
		return false, nil
	}

	if err := l.write(ctx); err != nil {
		return false, err
	}

	// Decorrelate the read-back so simultaneous acquirers do not confirm in
	// lockstep, then check whether our write is the one that survived.
	jitter := time.Duration(rand.Int64N(int64(leaseConfirmDelay)))
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(leaseConfirmDelay + jitter):
	}

	confirmed, exists, err := l.read(ctx)
	if err != nil {
		return false, err
	}
	if !exists || confirmed.Holder != l.holder {
		if l.logger != nil {
			l.logger.Debug("lease lost to another replica",
				zap.String("holder", confirmed.Holder),
				zap.String("self", l.holder),
			)
		}
		return false, nil
	}

	return true, nil
}
