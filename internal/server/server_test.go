package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/otoru/heimdall/internal/metrics"
	"github.com/otoru/heimdall/internal/storage"
	"go.uber.org/zap/zaptest"
)

type mockStore struct {
	getResp     *s3.GetObjectOutput
	headResp    *s3.HeadObjectOutput
	getErr      error
	headErr     error
	putErr      error
	listResp    []storage.Entry
	listErr     error
	putKeys     []string
	putErrAfter int // if > 0, first putErrAfter puts succeed, then putErr is returned
}

func (m *mockStore) Get(ctx context.Context, key string) (*s3.GetObjectOutput, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.getResp, nil
}

func (m *mockStore) Head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	if m.headErr != nil {
		return nil, m.headErr
	}
	return m.headResp, nil
}

func (m *mockStore) Put(ctx context.Context, key string, body io.ReadSeeker, contentType string, contentLength int64) error {
	m.putKeys = append(m.putKeys, key)
	if m.putErr != nil && (m.putErrAfter == 0 || len(m.putKeys) > m.putErrAfter) {
		return m.putErr
	}
	return nil
}

func (m *mockStore) List(ctx context.Context, prefix string, limit int32) ([]storage.Entry, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.listResp, nil
}

func (m *mockStore) GenerateChecksums(ctx context.Context, prefix string) error {
	return nil
}

func (m *mockStore) CleanupBadChecksums(ctx context.Context, prefix string) error {
	return nil
}

func (m *mockStore) Delete(ctx context.Context, key string) error {
	return nil
}

type listStore struct {
	listByPrefix map[string][]storage.Entry
	objects      map[string][]byte
}

func newListStore() *listStore {
	return &listStore{
		listByPrefix: make(map[string][]storage.Entry),
		objects:      make(map[string][]byte),
	}
}

func (s *listStore) Get(ctx context.Context, key string) (*s3.GetObjectOutput, error) {
	if b, ok := s.objects[key]; ok {
		return &s3.GetObjectOutput{
			Body:          io.NopCloser(bytes.NewReader(b)),
			ContentLength: aws.Int64(int64(len(b))),
			ContentType:   aws.String("application/json"),
		}, nil
	}
	return nil, fmt.Errorf("NotFound")
}

func (s *listStore) Head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	if b, ok := s.objects[key]; ok {
		return &s3.HeadObjectOutput{
			ContentLength: aws.Int64(int64(len(b))),
			ContentType:   aws.String("application/json"),
		}, nil
	}
	return nil, fmt.Errorf("NotFound")
}

func (s *listStore) Put(ctx context.Context, key string, body io.ReadSeeker, contentType string, contentLength int64) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.objects[key] = data
	return nil
}

func (s *listStore) List(ctx context.Context, prefix string, limit int32) ([]storage.Entry, error) {
	if entries, ok := s.listByPrefix[prefix]; ok {
		return entries, nil
	}
	return nil, nil
}

func (s *listStore) GenerateChecksums(ctx context.Context, prefix string) error { return nil }
func (s *listStore) CleanupBadChecksums(ctx context.Context, prefix string) error {
	return nil
}
func (s *listStore) Delete(ctx context.Context, key string) error { delete(s.objects, key); return nil }

// keyedMockStore wraps listStore and injects errors for specific keys.
type keyedMockStore struct {
	*listStore
	headErrByKey map[string]error
	getErrByKey  map[string]error
}

func (k *keyedMockStore) Get(ctx context.Context, key string) (*s3.GetObjectOutput, error) {
	if err, ok := k.getErrByKey[key]; ok {
		return nil, err
	}
	return k.listStore.Get(ctx, key)
}

func (k *keyedMockStore) Head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	if err, ok := k.headErrByKey[key]; ok {
		return nil, err
	}
	return k.listStore.Head(ctx, key)
}

func TestHandleGetOK(t *testing.T) {
	store := &mockStore{
		getResp: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader("hello")),
			ContentType:   aws.String("text/plain"),
			ContentLength: aws.Int64(5),
			ETag:          aws.String("\"etag\""),
		},
	}

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if got := rr.Body.String(); got != "hello" {
		t.Fatalf("unexpected body: %q", got)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("unexpected content-type: %s", ct)
	}
	if rr.Header().Get("ETag") != "etag" {
		t.Fatalf("unexpected etag header")
	}
}

func TestHandleHeadOK(t *testing.T) {
	store := &mockStore{
		headResp: &s3.HeadObjectOutput{
			ContentLength: aws.Int64(10),
			ContentType:   aws.String("application/java-archive"),
		},
	}

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodHead, "/path/to/artifact", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("expected empty body on HEAD")
	}
	if rr.Header().Get("Content-Length") != "10" {
		t.Fatalf("unexpected content-length header")
	}
}

func TestHandlePutOK(t *testing.T) {
	store := &mockStore{}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodPut, "/path/to/artifact", strings.NewReader("data"))
	req.Header.Set("Content-Type", "application/java-archive")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", rr.Code)
	}
	if len(store.putKeys) != 3 {
		t.Fatalf("expected 3 puts (artifact + checksums), got %d", len(store.putKeys))
	}
}

func TestAuthRequired(t *testing.T) {
	store := &mockStore{}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "user", "pass", "", "")
	req := httptest.NewRequest(http.MethodPut, "/secure/artifact", strings.NewReader("data"))
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}
func TestHandleGetNotFound(t *testing.T) {
	store := &mockStore{
		getErr: errors.New("NotFound"),
	}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rr.Code)
	}
}

func TestWriteErrorProxyStatus(t *testing.T) {
	rr := httptest.NewRecorder()
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.writeError(rr, "proxy fetch", ProxyStatusError{Code: http.StatusForbidden})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestWriteErrorProxyStatusPointer(t *testing.T) {
	rr := httptest.NewRecorder()
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	err := fmt.Errorf("wrapped: %w", ProxyStatusError{Code: http.StatusUnauthorized})
	srv.writeError(rr, "proxy fetch", err)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestWriteErrorCanceled(t *testing.T) {
	rr := httptest.NewRecorder()
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	_, cancel := context.WithCancel(context.Background())
	cancel()
	srv.writeError(rr, "fetch object", context.Canceled)
	if rr.Code != 499 {
		t.Fatalf("expected 499, got %d", rr.Code)
	}
}

func TestMetricsIncrement(t *testing.T) {
	m := metrics.New()
	store := &mockStore{
		headResp: &s3.HeadObjectOutput{},
	}
	srv := New(store, zaptest.NewLogger(t), m, "", "", "", "")
	req := httptest.NewRequest(http.MethodHead, "/metric-check", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "heimdall_http_requests_total" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected heimdall_http_requests_total metric to be present")
	}
}

func TestCatalogOK(t *testing.T) {
	store := &mockStore{
		listResp: []storage.Entry{
			{Name: "a.jar", Path: "releases/a.jar", Type: "file"},
			{Name: "b/", Path: "releases/b/", Type: "dir"},
		},
	}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/catalog?path=releases&limit=2", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected json content type, got %s", ct)
	}
	if !strings.Contains(rr.Body.String(), "a.jar") || !strings.Contains(rr.Body.String(), "b/") {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestCatalogRootShowsGroupAndFiltersProxyCfg(t *testing.T) {
	store := newListStore()
	store.listByPrefix[""] = []storage.Entry{
		{Name: "__proxycfg__/", Path: "__proxycfg__/", Type: "dir"},
		{Name: "local/", Path: "local/", Type: "dir"},
	}
	store.listByPrefix[proxyConfigPrefix] = []storage.Entry{
		{Name: "central.json", Path: "__proxycfg__/central.json", Type: "file"},
	}
	store.objects["__proxycfg__/central.json"] = []byte(`{"name":"central","url":"https://repo.maven.apache.org/maven2"}`)

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/catalog", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", rr.Code)
	}
	var entries []storage.Entry
	if err := json.NewDecoder(rr.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Path, "__proxycfg__") {
			t.Fatalf("proxy config leaked in catalog: %+v", e)
		}
	}
	foundGroup := false
	for _, e := range entries {
		if e.Path == "packages/" && e.Type == "group" {
			foundGroup = true
		}
	}
	if !foundGroup {
		t.Fatalf("packages group not found in catalog root")
	}
}

func TestCatalogPackagesFiltersProxyCfg(t *testing.T) {
	store := newListStore()
	store.listByPrefix[""] = []storage.Entry{
		{Name: "__proxycfg__/", Path: "__proxycfg__/", Type: "dir"},
		{Name: "local/", Path: "local/", Type: "dir"},
	}

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t)) // no proxies configured

	req := httptest.NewRequest(http.MethodGet, "/catalog?path=packages", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", rr.Code)
	}
	var entries []storage.Entry
	if err := json.NewDecoder(rr.Body).Decode(&entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Path, "__proxycfg__") {
			t.Fatalf("proxy config leaked in packages catalog: %+v", e)
		}
		if e.Type == "dir" && !strings.HasSuffix(e.Name, "/") {
			t.Fatalf("dir missing trailing slash: %+v", e)
		}
	}
}

func TestPackagesGetLocal(t *testing.T) {
	store := newListStore()
	store.objects["com/acme/app/1.0/app-1.0.jar"] = []byte("LOCAL")
	store.listByPrefix[""] = []storage.Entry{{Name: "root/", Path: "root/", Type: "dir"}}

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/packages/com/acme/app/1.0/app-1.0.jar", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if body := rr.Body.String(); body != "LOCAL" {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestPackagesGetCachedProxy(t *testing.T) {
	store := newListStore()
	store.listByPrefix["__proxycfg__/"] = []storage.Entry{
		{Name: "central.json", Path: "__proxycfg__/central.json", Type: "file"},
	}
	store.objects["__proxycfg__/central.json"] = []byte(`{"name":"central","url":"https://repo.maven.apache.org/maven2"}`)
	key := "com/acme/app/1.0/app-1.0.jar"
	store.objects["central/"+key] = []byte("CACHED")
	store.listByPrefix[""] = []storage.Entry{{Name: "central/", Path: "central/", Type: "dir"}}

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/packages/"+key, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if body := rr.Body.String(); body != "CACHED" {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestPackagesHeadLocal(t *testing.T) {
	store := newListStore()
	store.objects["com/acme/app/1.0/app-1.0.jar"] = []byte("LOCAL")
	store.listByPrefix[""] = []storage.Entry{{Name: "root/", Path: "root/", Type: "dir"}}

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodHead, "/packages/com/acme/app/1.0/app-1.0.jar", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("expected empty body on HEAD")
	}
	if rr.Header().Get("Content-Length") == "" {
		t.Fatalf("expected content-length header")
	}
}

func newProxyListStore(proxyName, proxyURL string) *listStore {
	store := newListStore()
	store.listByPrefix[proxyConfigPrefix] = []storage.Entry{
		{Name: proxyName + ".json", Path: proxyConfigPrefix + proxyName + ".json", Type: "file"},
	}
	cfg, _ := json.Marshal(Proxy{Name: proxyName, URL: proxyURL})
	store.objects[proxyConfigPrefix+proxyName+".json"] = cfg
	return store
}

func newAPIKeyServer(t *testing.T, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/licenses/valid" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(statusCode)
	}))
}

func newObjectStore() *mockStore {
	return &mockStore{
		getResp: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader("data")),
			ContentType:   aws.String("text/plain"),
			ContentLength: aws.Int64(4),
		},
	}
}

func TestAuthAPIKeyValid(t *testing.T) {
	ks := newAPIKeyServer(t, http.StatusOK)
	defer ks.Close()

	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "", "", ks.URL, "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.Header.Set("X-API-Key", "secret")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestAuthAPIKeyInvalid(t *testing.T) {
	ks := newAPIKeyServer(t, http.StatusForbidden)
	defer ks.Close()

	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "", "", ks.URL, "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.Header.Set("X-API-Key", "bad-key")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuthAPIKeyEndpointError(t *testing.T) {
	ks := newAPIKeyServer(t, http.StatusInternalServerError)
	defer ks.Close()

	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "", "", ks.URL, "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.Header.Set("X-API-Key", "any-key")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuthAPIKeyNetworkError(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()

	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "", "", dead.URL, "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.Header.Set("X-API-Key", "any-key")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuthAPIKeyEndpointNotConfigured(t *testing.T) {
	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "user", "pass", "", "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.Header.Set("X-API-Key", "any-key")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuthBasicAuthStillWorks(t *testing.T) {
	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "user", "pass", "", "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.SetBasicAuth("user", "pass")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestAuthBothConfigured_APIKey(t *testing.T) {
	ks := newAPIKeyServer(t, http.StatusOK)
	defer ks.Close()

	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "user", "pass", ks.URL, "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.Header.Set("X-API-Key", "valid-key")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestAuthBothConfigured_Basic(t *testing.T) {
	ks := newAPIKeyServer(t, http.StatusOK)
	defer ks.Close()

	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "user", "pass", ks.URL, "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.SetBasicAuth("user", "pass")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestAuthNoAuthConfigured(t *testing.T) {
	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestAuthAPIKeyTokenSentInHeader(t *testing.T) {
	const wantToken = "super-secret-token"
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ks.Close()

	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "", "", ks.URL, wantToken)
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.Header.Set("X-API-Key", "any-key")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (token accepted), got %d", rr.Code)
	}
}

func TestAuthAPIKeyTokenMissing(t *testing.T) {
	// endpoint requires a token but none is configured → 401
	ks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ks.Close()

	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "", "", ks.URL, "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.Header.Set("X-API-Key", "any-key")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestAuthAPIKeyInvalidEndpointURL(t *testing.T) {
	// "http://[::1" has an unclosed bracket — url.Parse returns an error
	srv := New(newObjectStore(), zaptest.NewLogger(t), metrics.New(), "", "", "http://[::1", "")
	req := httptest.NewRequest(http.MethodGet, "/path/to/artifact", nil)
	req.Header.Set("X-API-Key", "any-key")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestHandleHealth(t *testing.T) {
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if body := rr.Body.String(); body != "ok" {
		t.Fatalf("expected body 'ok', got %q", body)
	}
}

func TestHandleObjectEmptyKey(t *testing.T) {
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestHandleObjectMethodNotAllowed(t *testing.T) {
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodDelete, "/some/artifact.jar", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != "GET, HEAD, PUT" {
		t.Fatalf("expected Allow: GET, HEAD, PUT, got %q", allow)
	}
}

func TestHandleHeadNotFoundNoProxy(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/missing/artifact.jar", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestHandleHeadFoundViaProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "42")
		w.Header().Set("Content-Type", "application/java-archive")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/central/com/acme/app-1.0.jar", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if cl := rr.Header().Get("Content-Length"); cl != "42" {
		t.Fatalf("expected Content-Length 42, got %q", cl)
	}
}

func TestHandlePackagesEmptyKey(t *testing.T) {
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/packages/", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestHandlePackagesMethodNotAllowed(t *testing.T) {
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodPut, "/packages/com/acme/foo.jar", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Fatalf("expected Allow: GET, HEAD, got %q", allow)
	}
}

func TestHandlePackageGetNotFound(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/packages/com/acme/missing-1.0.jar", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestHandleListProxiesEmpty(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/proxies", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestHandleListProxiesWithEntries(t *testing.T) {
	store := newProxyListStore("central", "http://example.com")
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/proxies", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var proxies []Proxy
	if err := json.NewDecoder(rr.Body).Decode(&proxies); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(proxies) != 1 || proxies[0].Name != "central" {
		t.Fatalf("unexpected proxies: %+v", proxies)
	}
}

func TestHandleCreateProxyOK(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	body := strings.NewReader(`{"name":"my-proxy","url":"http://example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/proxies", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rr.Code)
	}
}

func TestHandleCreateProxyInvalidJSON(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodPost, "/proxies", strings.NewReader("not json"))
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestHandleCreateProxyValidationError(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	body := strings.NewReader(`{"name":"invalid name!","url":"http://example.com"}`)
	req := httptest.NewRequest(http.MethodPost, "/proxies", body)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestHandleUpdateProxyOK(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	body := strings.NewReader(`{"url":"http://new-url.example.com"}`)
	req := httptest.NewRequest(http.MethodPut, "/proxies/myproxy", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestHandleUpdateProxyInvalidJSON(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodPut, "/proxies/myproxy", strings.NewReader("bad json"))
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestHandleDeleteProxyOK(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodDelete, "/proxies/central", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rr.Code)
	}
}

func TestRouteProxiesMethodNotAllowed(t *testing.T) {
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")

	req := httptest.NewRequest(http.MethodDelete, "/proxies", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != "GET, POST" {
		t.Fatalf("expected Allow: GET, POST, got %q", allow)
	}
}

func TestRouteProxyByNameMethodNotAllowed(t *testing.T) {
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")

	req := httptest.NewRequest(http.MethodGet, "/proxies/central", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != "PUT, DELETE" {
		t.Fatalf("expected Allow: PUT, DELETE, got %q", allow)
	}
}

func TestRouteProxyByNameEmptyName(t *testing.T) {
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")

	req := httptest.NewRequest(http.MethodGet, "/proxies/", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestRunChecksumScanner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &mockStore{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		RunChecksumScanner(ctx, zaptest.NewLogger(t), store, "", time.Millisecond)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scanner did not stop after context cancellation")
	}
}

func TestHandlePackageHeadLocalDirect(t *testing.T) {
	store := newListStore()
	store.objects["app.jar"] = []byte("content")
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/packages/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestHandlePackageHeadCachedProxy(t *testing.T) {
	store := newProxyListStore("central", "http://not-used")
	store.objects["central/com/acme/app-1.0.jar"] = []byte("CACHED")
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/packages/com/acme/app-1.0.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestHandlePackageHeadViaUpstreamProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "50")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/packages/com/acme/missing.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if cl := rr.Header().Get("Content-Length"); cl != "50" {
		t.Fatalf("expected Content-Length 50, got %q", cl)
	}
}

func TestHandlePackageHeadNotFound(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/packages/nonexistent.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestHandlePackageGetViaUpstream(t *testing.T) {
	const content = "UPSTREAM-CONTENT"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/java-archive")
		fmt.Fprint(w, content)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/packages/com/acme/app-1.0.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Body.String() != content {
		t.Fatalf("expected %q, got %q", content, rr.Body.String())
	}
}

func TestWriteErrorNotFound(t *testing.T) {
	rr := httptest.NewRecorder()
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.writeError(rr, "fetch", errors.New("NotFound"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestWriteErrorInternalError(t *testing.T) {
	rr := httptest.NewRecorder()
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.writeError(rr, "fetch", errors.New("unexpected failure"))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestWriteErrorDeadlineExceeded(t *testing.T) {
	rr := httptest.NewRecorder()
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.writeError(rr, "fetch", context.DeadlineExceeded)
	if rr.Code != 499 {
		t.Fatalf("expected 499, got %d", rr.Code)
	}
}

func TestHandleListProxiesError(t *testing.T) {
	store := &mockStore{listErr: errors.New("storage error")}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/proxies", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestHandleDeleteProxyInvalidName(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodDelete, "/proxies/invalid!name", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestHandlePutNoContentLength(t *testing.T) {
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodPut, "/artifact.jar", strings.NewReader("data"))
	req.ContentLength = -1
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusLengthRequired {
		t.Fatalf("expected 411, got %d", rr.Code)
	}
}

func TestHandlePutStoreError(t *testing.T) {
	store := &mockStore{putErr: errors.New("storage error")}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodPut, "/artifact.jar", strings.NewReader("data"))
	req.Header.Set("Content-Type", "application/java-archive")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestHandlePutSha1StoreError(t *testing.T) {
	store := &mockStore{putErr: errors.New("sha1 error"), putErrAfter: 1}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodPut, "/artifact.jar", strings.NewReader("data"))
	req.ContentLength = 4
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestHandlePutMd5StoreError(t *testing.T) {
	store := &mockStore{putErr: errors.New("md5 error"), putErrAfter: 2}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodPut, "/artifact.jar", strings.NewReader("data"))
	req.ContentLength = 4
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestHandleGetFetchAndCacheError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/central/artifact.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestHandleGetFetchAndCacheFound(t *testing.T) {
	const content = "PROXIED"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/java-archive")
		fmt.Fprint(w, content)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/central/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Body.String() != content {
		t.Fatalf("expected %q, got %q", content, rr.Body.String())
	}
}

func TestHandleHeadViaProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "99")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/central/artifact.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if cl := rr.Header().Get("Content-Length"); cl != "99" {
		t.Fatalf("expected Content-Length 99, got %q", cl)
	}
}

func TestHandleHeadInternalError(t *testing.T) {
	store := &mockStore{headErr: errors.New("internal error")}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodHead, "/artifact.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestHandleHeadViaProxyError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/central/artifact.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestCatalogWithProxyMerge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><a href="a-1.0.jar">a-1.0.jar</a></body></html>`)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/catalog?path=central", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "a-1.0.jar") {
		t.Fatalf("expected proxy entries in catalog, body: %s", rr.Body.String())
	}
}

func TestCatalogPackagesWithProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><a href="com/">com/</a></body></html>`)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/catalog?path=packages", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestHandlePackageGetCachedProxyError(t *testing.T) {
	base := newProxyListStore("central", "http://unused")
	store := &keyedMockStore{
		listStore:   base,
		getErrByKey: map[string]error{"central/com/acme/app.jar": errors.New("storage internal error")},
	}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/packages/com/acme/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestHandlePackageHeadCachedProxyError(t *testing.T) {
	base := newProxyListStore("central", "http://unused")
	store := &keyedMockStore{
		listStore:    base,
		headErrByKey: map[string]error{"central/com/acme/app.jar": errors.New("storage internal error")},
	}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/packages/com/acme/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestHandlePackageGetFetchFromAnyError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/packages/com/acme/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestHandlePackageGetProxyUnauthorized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/packages/com/acme/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestHandlePackageHeadFetchFromAnyError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/packages/com/acme/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestHandlePackageHeadProxyUnauthorized(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	store := newProxyListStore("central", upstream.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/packages/com/acme/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestTryLocalHeadViaDirectory(t *testing.T) {
	store := newListStore()
	store.listByPrefix[""] = []storage.Entry{
		{Name: "readme.txt", Path: "readme.txt", Type: "file"}, // non-dir: skipped
		{Name: "releases/", Path: "releases/", Type: "dir"},
	}
	store.objects["releases/app.jar"] = []byte("content")

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/packages/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

func TestHandleGetInternalError(t *testing.T) {
	store := &mockStore{getErr: errors.New("internal storage failure")}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/artifact.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rr.Code)
	}
}

func TestWriteHeadResponseLastModified(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	etag := `"abc123"`
	ct := "application/java-archive"
	store := &mockStore{
		headResp: &s3.HeadObjectOutput{
			ContentLength: aws.Int64(42),
			ContentType:   &ct,
			ETag:          &etag,
			LastModified:  &now,
		},
	}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodHead, "/file.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("ETag") != "abc123" {
		t.Fatalf("expected ETag abc123, got %q", rr.Header().Get("ETag"))
	}
	if rr.Header().Get("Last-Modified") == "" {
		t.Fatalf("expected Last-Modified header")
	}
}

func TestWriteObjectResponseLastModified(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	etag := `"xyz"`
	store := &mockStore{
		getResp: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader("data")),
			ContentLength: aws.Int64(4),
			ETag:          &etag,
			LastModified:  &now,
		},
	}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/file.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("Last-Modified") == "" {
		t.Fatalf("expected Last-Modified header")
	}
}

func TestHandleHeadSingleComponentKey(t *testing.T) {
	// key with no slash: proxy.Head's splitProxyKey returns ok=false
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodHead, "/singlefile.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rr.Code)
	}
}

func TestCatalogMaybeListProxyNetworkError(t *testing.T) {
	// Proxy configured but upstream is dead → maybeListProxy returns error → logged, not fatal
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()

	store := newProxyListStore("central", dead.URL)
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/catalog?path=central", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (error logged, not fatal), got %d", rr.Code)
	}
}

func TestCatalogLimitOutOfRange(t *testing.T) {
	store := &mockStore{listResp: []storage.Entry{{Name: "a.jar", Path: "a.jar", Type: "file"}}}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")

	// limit=9999 is out of range (max 1000), default of 100 is used
	req := httptest.NewRequest(http.MethodGet, "/catalog?limit=9999", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
}

// writeHeadResponse and writeObjectResponse are used by handlePackageHead/Get, not handleHead/Get.
// The following tests target those functions specifically.

func TestPackageHeadAllHeaders(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	etag := `"abc"`
	ct := "application/java-archive"
	store := &mockStore{
		headResp: &s3.HeadObjectOutput{
			ContentLength: aws.Int64(42),
			ContentType:   &ct,
			ETag:          &etag,
			LastModified:  &now,
		},
	}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodHead, "/packages/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("ETag") != "abc" {
		t.Fatalf("expected ETag abc, got %q", rr.Header().Get("ETag"))
	}
	if rr.Header().Get("Last-Modified") == "" {
		t.Fatalf("expected Last-Modified header")
	}
}

func TestPackageGetAllHeaders(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	etag := `"xyz"`
	ct := "application/java-archive"
	store := &mockStore{
		getResp: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader("content")),
			ContentLength: aws.Int64(7),
			ContentType:   &ct,
			ETag:          &etag,
			LastModified:  &now,
		},
	}
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	req := httptest.NewRequest(http.MethodGet, "/packages/app.jar", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if rr.Header().Get("Last-Modified") == "" {
		t.Fatalf("expected Last-Modified header")
	}
}

func TestHandleUpdateProxyValidationError(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodPut, "/proxies/invalid!proxy", strings.NewReader(`{"url":"http://example.com"}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestHandleCreateProxyEmptyURL(t *testing.T) {
	store := newListStore()
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodPost, "/proxies", strings.NewReader(`{"name":"central","url":""}`))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestHandleListProxiesInvalidJSON(t *testing.T) {
	// proxy config with invalid JSON → load returns error → proxy skipped → empty list returned
	store := newListStore()
	store.listByPrefix["__proxycfg__/"] = []storage.Entry{
		{Name: "central.json", Path: "__proxycfg__/central.json", Type: "file"},
	}
	store.objects["__proxycfg__/central.json"] = []byte("not valid json{")

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/proxies", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (bad proxies skipped), got %d", rr.Code)
	}
	var proxies []Proxy
	if err := json.NewDecoder(rr.Body).Decode(&proxies); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(proxies) != 0 {
		t.Fatalf("expected empty proxy list, got %d", len(proxies))
	}
}

func TestHandleListProxiesNonFileEntries(t *testing.T) {
	// non-file and non-json entries in proxy config dir should be skipped
	store := newListStore()
	store.listByPrefix["__proxycfg__/"] = []storage.Entry{
		{Name: "subdir/", Path: "__proxycfg__/subdir/", Type: "dir"},
		{Name: "README", Path: "__proxycfg__/README", Type: "file"},
		{Name: "central.json", Path: "__proxycfg__/central.json", Type: "file"},
	}
	store.objects["__proxycfg__/central.json"] = []byte(`{"name":"central","url":"http://example.com"}`)

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "", "", "")
	srv.proxy = NewProxyManager(store, zaptest.NewLogger(t))

	req := httptest.NewRequest(http.MethodGet, "/proxies", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var proxies []Proxy
	if err := json.NewDecoder(rr.Body).Decode(&proxies); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(proxies) != 1 || proxies[0].Name != "central" {
		t.Fatalf("expected 1 proxy named central, got %+v", proxies)
	}
}
