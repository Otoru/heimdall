package server

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/otoru/heimdall/internal/metrics"
	"github.com/otoru/heimdall/internal/storage"
	"go.uber.org/zap/zaptest"
)

// flakyStore wraps memStore so a test can fail specific operations on demand
// and observe the order in which keys are written.
type flakyStore struct {
	*memStore

	mu        sync.Mutex
	putOrder  []string
	listCalls int32
	getCalls  int32

	// failGet fails Get for keys matching this prefix with a non-NotFound
	// error, emulating a transient object storage failure.
	failGetPrefix atomic.Value // string
}

func newFlakyStore() *flakyStore {
	f := &flakyStore{memStore: newMemStore()}
	f.failGetPrefix.Store("")
	return f
}

func (f *flakyStore) setFailGetPrefix(p string) { f.failGetPrefix.Store(p) }

func (f *flakyStore) Get(ctx context.Context, key string) (*s3.GetObjectOutput, error) {
	atomic.AddInt32(&f.getCalls, 1)
	if p := f.failGetPrefix.Load().(string); p != "" && strings.HasPrefix(key, p) {
		return nil, errors.New("connection reset by peer")
	}
	return f.memStore.Get(ctx, key)
}

func (f *flakyStore) List(ctx context.Context, prefix string, limit int32) ([]storage.Entry, error) {
	atomic.AddInt32(&f.listCalls, 1)
	return f.memStore.List(ctx, prefix, limit)
}

func (f *flakyStore) Put(ctx context.Context, key string, body io.ReadSeeker, contentType string, contentLength int64) error {
	f.mu.Lock()
	f.putOrder = append(f.putOrder, key)
	f.mu.Unlock()
	return f.memStore.Put(ctx, key, body, contentType, contentLength)
}

func (f *flakyStore) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.putOrder...)
}

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

// ageSnapshot pushes the cached proxy list past its TTL without discarding it,
// which is what expiry looks like in production. invalidate() is a different
// thing: it drops the snapshot outright and is only used after a mutation.
func ageSnapshot(t *testing.T, c *proxyCache, age time.Duration) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap == nil {
		t.Fatal("no snapshot to age; warm the cache first")
	}
	c.snap.loadedAt = c.snap.loadedAt.Add(-age)
}

// TestProxyListServesStaleOnTransientConfigReadFailure is the regression test
// for the phantom 404s: a single failed read of a proxy definition used to drop
// that upstream from the list, so artifacts cached under it stopped resolving.
func TestProxyListServesStaleOnTransientConfigReadFailure(t *testing.T) {
	store := newFlakyStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	ctx := context.Background()

	if err := pm.Add(ctx, Proxy{Name: "central", URL: "https://repo.maven.apache.org/maven2"}); err != nil {
		t.Fatalf("add proxy: %v", err)
	}
	if list, err := pm.List(ctx); err != nil || len(list) != 1 {
		t.Fatalf("warm-up list: got %d proxies, err=%v", len(list), err)
	}

	// Storage starts failing config reads while the snapshot goes stale. The
	// list must not shrink.
	store.setFailGetPrefix(proxyConfigPrefix)
	ageSnapshot(t, pm.cache, time.Minute)

	list, err := pm.List(ctx)
	if err != nil {
		t.Fatalf("expected stale list to be served, got error: %v", err)
	}
	if len(list) != 1 || list[0].Name != "central" {
		t.Fatalf("expected the cached central proxy, got %+v", list)
	}
}

func TestProxyListFailsWhenConfigUnreadableAndNothingCached(t *testing.T) {
	store := newFlakyStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	ctx := context.Background()

	if err := pm.Add(ctx, Proxy{Name: "central", URL: "https://example.com"}); err != nil {
		t.Fatalf("add proxy: %v", err)
	}
	store.setFailGetPrefix(proxyConfigPrefix)
	pm.cache.invalidate()

	// With no previous snapshot to fall back on, the error must surface rather
	// than be laundered into an empty list (which the caller reports as 404).
	if _, err := pm.List(ctx); err == nil {
		t.Fatal("expected error when config is unreadable and nothing is cached")
	}
}

func TestProxyListIsMemoized(t *testing.T) {
	store := newFlakyStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	ctx := context.Background()

	if err := pm.Add(ctx, Proxy{Name: "central", URL: "https://example.com"}); err != nil {
		t.Fatalf("add proxy: %v", err)
	}

	for i := 0; i < 50; i++ {
		if _, err := pm.List(ctx); err != nil {
			t.Fatalf("list %d: %v", i, err)
		}
	}

	if got := atomic.LoadInt32(&store.listCalls); got != 1 {
		t.Fatalf("expected 1 storage LIST for 50 calls, got %d", got)
	}
}

func TestProxyListInvalidatedOnMutation(t *testing.T) {
	store := newFlakyStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	ctx := context.Background()

	if err := pm.Add(ctx, Proxy{Name: "central", URL: "https://example.com"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if list, _ := pm.List(ctx); len(list) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(list))
	}
	if err := pm.Add(ctx, Proxy{Name: "internal", URL: "https://example.org"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if list, _ := pm.List(ctx); len(list) != 2 {
		t.Fatalf("expected the new proxy to be visible immediately, got %d", len(list))
	}
	if err := pm.Delete(ctx, "internal"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if list, _ := pm.List(ctx); len(list) != 1 {
		t.Fatalf("expected the deleted proxy to disappear immediately, got %d", len(list))
	}
}

func TestProxyListCollapsesConcurrentRefreshes(t *testing.T) {
	store := newFlakyStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	ctx := context.Background()

	if err := pm.Add(ctx, Proxy{Name: "central", URL: "https://example.com"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	pm.cache.invalidate()
	atomic.StoreInt32(&store.listCalls, 0)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pm.List(ctx); err != nil {
				t.Errorf("concurrent list: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&store.listCalls); got > 2 {
		t.Fatalf("expected concurrent refreshes to collapse, got %d storage LISTs", got)
	}
}

func TestFetchAndCacheRetriesTransientUpstreamFailure(t *testing.T) {
	body := []byte("JARCONTENT")
	var hits int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha1") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Fail the first two attempts the way an overloaded upstream would.
		if atomic.AddInt32(&hits, 1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write(body)
	}))
	defer remote.Close()

	store := newFlakyStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	ctx := context.Background()
	if err := pm.Add(ctx, Proxy{Name: "central", URL: remote.URL}); err != nil {
		t.Fatalf("add: %v", err)
	}

	found, err := pm.FetchAndCache(ctx, "central/com/acme/app/1.0/app-1.0.jar")
	if err != nil {
		t.Fatalf("expected retries to absorb the 502s, got: %v", err)
	}
	if !found {
		t.Fatal("expected the artifact to be cached")
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("expected 3 upstream attempts, got %d", got)
	}
}

func TestFetchAndCacheRejectsUpstreamChecksumMismatch(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha1") {
			// Upstream advertises a digest for content we will not receive.
			_, _ = w.Write([]byte(sha1Hex([]byte("THE REAL JAR"))))
			return
		}
		_, _ = w.Write([]byte("TRUNCATED"))
	}))
	defer remote.Close()

	store := newFlakyStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	ctx := context.Background()
	if err := pm.Add(ctx, Proxy{Name: "central", URL: remote.URL}); err != nil {
		t.Fatalf("add: %v", err)
	}

	key := "central/com/acme/app/1.0/app-1.0.jar"
	found, err := pm.FetchAndCache(ctx, key)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got found=%v err=%v", found, err)
	}
	if found {
		t.Fatal("corrupt artifact must not be reported as cached")
	}
	if _, ok := store.data[key]; ok {
		t.Fatal("corrupt artifact must not be written to storage")
	}
}

func TestFetchAndCacheAcceptsMatchingUpstreamChecksum(t *testing.T) {
	body := []byte("JARCONTENT")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha1") {
			// sha1sum-style payload, which is what several mirrors serve.
			_, _ = fmt.Fprintf(w, "%s  app-1.0.jar\n", sha1Hex(body))
			return
		}
		_, _ = w.Write(body)
	}))
	defer remote.Close()

	store := newFlakyStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	ctx := context.Background()
	if err := pm.Add(ctx, Proxy{Name: "central", URL: remote.URL}); err != nil {
		t.Fatalf("add: %v", err)
	}

	key := "central/com/acme/app/1.0/app-1.0.jar"
	found, err := pm.FetchAndCache(ctx, key)
	if err != nil || !found {
		t.Fatalf("expected the artifact to be cached, got found=%v err=%v", found, err)
	}
}

// TestFetchAndCacheWritesChecksumsBeforeArtifact pins the write ordering.
// Readers resolve the artifact first, so publishing it before its checksum
// leaves a window where Maven downloads the jar and validates it against a
// stale or missing digest.
func TestFetchAndCacheWritesChecksumsBeforeArtifact(t *testing.T) {
	body := []byte("JARCONTENT")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha1") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	defer remote.Close()

	store := newFlakyStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	ctx := context.Background()
	if err := pm.Add(ctx, Proxy{Name: "central", URL: remote.URL}); err != nil {
		t.Fatalf("add: %v", err)
	}

	key := "central/com/acme/app/1.0/app-1.0.jar"
	if _, err := pm.FetchAndCache(ctx, key); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	writes := store.writes()
	posOf := func(k string) int {
		for i, w := range writes {
			if w == k {
				return i
			}
		}
		return -1
	}
	artifact, sha, md5 := posOf(key), posOf(key+".sha1"), posOf(key+".md5")
	if artifact < 0 || sha < 0 || md5 < 0 {
		t.Fatalf("expected artifact and both checksums to be written, got %v", writes)
	}
	if sha > artifact || md5 > artifact {
		t.Fatalf("checksums must be written before the artifact, got order %v", writes)
	}

	// And the published digest must describe the published bytes.
	obj, err := store.memStore.Get(ctx, key+".sha1")
	if err != nil {
		t.Fatalf("read cached sha1: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if string(got) != sha1Hex(body) {
		t.Fatalf("cached sha1 %q does not describe the cached artifact %q", got, sha1Hex(body))
	}
}

// TestPackageGetSurvivesTransientConfigReadFailure exercises the whole Maven
// path: an artifact already in the cache must keep resolving while the proxy
// definition store is misbehaving. This is the exact sequence that turned into
// "Could not find artifact ... in heimdall" for builds.
func TestPackageGetSurvivesTransientConfigReadFailure(t *testing.T) {
	store := newFlakyStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	srv.proxy = pm
	ctx := context.Background()

	if err := pm.Add(ctx, Proxy{Name: "central", URL: "https://repo.maven.apache.org/maven2"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Artifact is already cached under the proxy.
	artifact := "central/junit/junit/4.12/junit-4.12.jar"
	if err := store.memStore.Put(ctx, artifact, strings.NewReader("JAR"), "application/java-archive", 3); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if _, err := pm.List(ctx); err != nil {
		t.Fatalf("warm cache: %v", err)
	}

	// Now the config store starts failing and the snapshot goes stale, so the
	// request forces a refresh that cannot succeed.
	store.setFailGetPrefix(proxyConfigPrefix)
	ageSnapshot(t, pm.cache, time.Minute)

	req := httptest.NewRequest(http.MethodGet, "/packages/junit/junit/4.12/junit-4.12.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected the cached artifact to still be served, got %d", rr.Code)
	}
	if rr.Body.String() != "JAR" {
		t.Fatalf("unexpected body %q", rr.Body.String())
	}
}
