package server

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"go.uber.org/zap"
)

const (
	defaultRetryAttempts = 3
	defaultRetryBase     = 150 * time.Millisecond
	defaultRetryMax      = 2 * time.Second
)

// retryPolicy retries idempotent upstream requests. A Maven build resolves
// hundreds of artifacts per run, so a per-request error rate that looks
// negligible in isolation turns into a failed build most of the time. Upstream
// repositories and object stores both produce occasional transport errors and
// 5xx responses; none of them should reach the client on the first try.
type retryPolicy struct {
	attempts int
	base     time.Duration
	max      time.Duration
	logger   *zap.Logger
	onRetry  func()

	// sleep is overridable in tests to avoid real delays.
	sleep func(ctx context.Context, d time.Duration) error
}

func newRetryPolicy(attempts int, logger *zap.Logger, onRetry func()) retryPolicy {
	if attempts <= 0 {
		attempts = defaultRetryAttempts
	}
	if onRetry == nil {
		onRetry = func() {}
	}
	return retryPolicy{
		attempts: attempts,
		base:     defaultRetryBase,
		max:      defaultRetryMax,
		logger:   logger,
		onRetry:  onRetry,
		sleep:    sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryableStatus reports whether a response status is worth another attempt.
// 4xx responses other than 429 are deliberate answers from upstream and are
// never retried: retrying a 404 only slows the build down.
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

func (p retryPolicy) backoff(attempt int) time.Duration {
	d := p.base << attempt
	if d > p.max {
		d = p.max
	}
	// Full jitter: concurrent resolvers must not retry in lockstep.
	return time.Duration(rand.Int64N(int64(d)) + int64(d)/2)
}

// do issues req, retrying transport errors and retryable statuses. It is only
// safe for requests without a body to replay (GET/HEAD), which is all the proxy
// issues upstream.
func (p retryPolicy) do(client *http.Client, req *http.Request) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt < p.attempts; attempt++ {
		if attempt > 0 {
			p.onRetry()
			if err := p.sleep(req.Context(), p.backoff(attempt-1)); err != nil {
				return nil, err
			}
		}

		resp, err := client.Do(req.Clone(req.Context()))
		if err != nil {
			// A cancelled request is the client giving up, not a failure to
			// retry against.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			lastErr = err
			p.logRetry(req, attempt, zap.Error(err))
			continue
		}

		if attempt < p.attempts-1 && retryableStatus(resp.StatusCode) {
			drainAndClose(resp)
			lastErr = ProxyStatusError{Code: resp.StatusCode}
			p.logRetry(req, attempt, zap.Int("status", resp.StatusCode))
			continue
		}

		return resp, nil
	}

	return nil, lastErr
}

func (p retryPolicy) logRetry(req *http.Request, attempt int, field zap.Field) {
	if p.logger == nil {
		return
	}
	p.logger.Warn("upstream request failed; retrying",
		zap.String("url", req.URL.Redacted()),
		zap.Int("attempt", attempt+1),
		zap.Int("attempts", p.attempts),
		field,
	)
}

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
