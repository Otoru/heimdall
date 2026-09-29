package server

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

const (
	// defaultProxyCacheTTL is how long a loaded proxy list is trusted before
	// being refreshed from storage.
	defaultProxyCacheTTL = 30 * time.Second
	// defaultProxyCacheStaleGrace is how long a previously loaded list keeps
	// being served after a refresh failure. Proxy definitions change by
	// operator action only, so serving a slightly stale list is far safer
	// than degrading to an empty or partial one.
	defaultProxyCacheStaleGrace = 15 * time.Minute
)

// ProxyCacheResult describes how a call to proxyCache.get was satisfied. It is
// reported to the metrics hook so the cache can be observed in production.
type ProxyCacheResult string

const (
	ProxyCacheHit     ProxyCacheResult = "hit"
	ProxyCacheRefresh ProxyCacheResult = "refresh"
	ProxyCacheStale   ProxyCacheResult = "stale"
	ProxyCacheError   ProxyCacheResult = "error"
)

type proxySnapshot struct {
	proxies  []Proxy
	loadedAt time.Time
}

// proxyCache memoizes the proxy list that used to be re-read from object
// storage on every cache-missing artifact request. Besides the obvious cost
// (one LIST plus one GET per proxy definition, several times per request), the
// old behaviour made every transient storage error observable to clients: a
// proxy that failed to load was silently dropped from the list, which turned
// into a 404 for artifacts that exist upstream.
type proxyCache struct {
	ttl        time.Duration
	staleGrace time.Duration
	logger     *zap.Logger
	onResult   func(ProxyCacheResult)

	mu   sync.RWMutex
	snap *proxySnapshot

	group singleflight.Group

	// now is overridable in tests.
	now func() time.Time
}

func newProxyCache(ttl, staleGrace time.Duration, logger *zap.Logger, onResult func(ProxyCacheResult)) *proxyCache {
	if ttl <= 0 {
		ttl = defaultProxyCacheTTL
	}
	if staleGrace <= 0 {
		staleGrace = defaultProxyCacheStaleGrace
	}
	if onResult == nil {
		onResult = func(ProxyCacheResult) {}
	}
	return &proxyCache{
		ttl:        ttl,
		staleGrace: staleGrace,
		logger:     logger,
		onResult:   onResult,
		now:        time.Now,
	}
}

func (c *proxyCache) snapshot() *proxySnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snap
}

func (c *proxyCache) store(snap *proxySnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap = snap
}

// invalidate drops the cached list so the next read reloads from storage. It is
// called after any mutation of the proxy definitions.
func (c *proxyCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snap = nil
}

// get returns the cached proxy list, refreshing it through load when stale.
// Concurrent refreshes are collapsed into a single storage round trip.
//
// If the refresh fails but a previous snapshot is still within the stale grace
// window, that snapshot is returned instead of an error: an outage of the
// config store must not make cached artifacts disappear.
func (c *proxyCache) get(ctx context.Context, load func(context.Context) ([]Proxy, error)) ([]Proxy, error) {
	if snap := c.snapshot(); snap != nil && c.now().Sub(snap.loadedAt) < c.ttl {
		c.onResult(ProxyCacheHit)
		return snap.proxies, nil
	}

	v, err, _ := c.group.Do("proxies", func() (interface{}, error) {
		// Another goroutine may have refreshed while we waited for the flight.
		if snap := c.snapshot(); snap != nil && c.now().Sub(snap.loadedAt) < c.ttl {
			return snap, nil
		}

		proxies, err := load(ctx)
		if err != nil {
			return nil, err
		}
		snap := &proxySnapshot{proxies: proxies, loadedAt: c.now()}
		c.store(snap)
		return snap, nil
	})

	if err != nil {
		if snap := c.snapshot(); snap != nil && c.now().Sub(snap.loadedAt) < c.staleGrace {
			c.onResult(ProxyCacheStale)
			if c.logger != nil {
				c.logger.Warn("serving stale proxy list after refresh failure",
					zap.Error(err),
					zap.Duration("age", c.now().Sub(snap.loadedAt)),
					zap.Int("proxies", len(snap.proxies)),
				)
			}
			return snap.proxies, nil
		}
		c.onResult(ProxyCacheError)
		return nil, err
	}

	c.onResult(ProxyCacheRefresh)
	return v.(*proxySnapshot).proxies, nil
}
