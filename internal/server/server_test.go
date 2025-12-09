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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/otoru/heimdall/internal/metrics"
	"github.com/otoru/heimdall/internal/storage"
	"go.uber.org/zap/zaptest"
)

type mockStore struct {
	getResp  *s3.GetObjectOutput
	headResp *s3.HeadObjectOutput
	getErr   error
	headErr  error
	putErr   error
	listResp []storage.Entry
	listErr  error
	putKeys  []string
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
	return m.putErr
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

func TestHandleGetOK(t *testing.T) {
	store := &mockStore{
		getResp: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader("hello")),
			ContentType:   aws.String("text/plain"),
			ContentLength: aws.Int64(5),
			ETag:          aws.String("\"etag\""),
		},
	}

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "")
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

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "")
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
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "")
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
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "user", "pass")
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
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "")
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rr := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", rr.Code)
	}
}

func TestWriteErrorProxyStatus(t *testing.T) {
	rr := httptest.NewRecorder()
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "")
	srv.writeError(rr, "proxy fetch", ProxyStatusError{Code: http.StatusForbidden})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestWriteErrorProxyStatusPointer(t *testing.T) {
	rr := httptest.NewRecorder()
	srv := New(&mockStore{}, zaptest.NewLogger(t), metrics.New(), "", "")
	err := fmt.Errorf("wrapped: %w", ProxyStatusError{Code: http.StatusUnauthorized})
	srv.writeError(rr, "proxy fetch", err)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestMetricsIncrement(t *testing.T) {
	m := metrics.New()
	store := &mockStore{
		headResp: &s3.HeadObjectOutput{},
	}
	srv := New(store, zaptest.NewLogger(t), m, "", "")
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
	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "")
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

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "")
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

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "")
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

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "")
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

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "")
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

	srv := New(store, zaptest.NewLogger(t), metrics.New(), "", "")
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
