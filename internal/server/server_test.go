package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/otoru/heimdall/internal/metrics"
	"go.uber.org/zap/zaptest"
)

type mockStore struct {
	getResp  *s3.GetObjectOutput
	headResp *s3.HeadObjectOutput
	getErr   error
	headErr  error
	putErr   error
	listResp []string
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

func (m *mockStore) List(ctx context.Context, prefix string, limit int32) ([]string, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.listResp, nil
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
		listResp: []string{"a.jar", "b/"},
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
