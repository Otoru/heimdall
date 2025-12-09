package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/otoru/heimdall/internal/storage"
	"go.uber.org/zap/zaptest"
)

type memObj struct {
	body        []byte
	contentType string
}

type memStore struct {
	data map[string]memObj
}

func newMemStore() *memStore {
	return &memStore{data: make(map[string]memObj)}
}

func (m *memStore) Get(ctx context.Context, key string) (*s3.GetObjectOutput, error) {
	obj, ok := m.data[key]
	if !ok {
		return nil, errors.New("NotFound")
	}
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(obj.body)),
		ContentLength: aws.Int64(int64(len(obj.body))),
		ContentType:   aws.String(obj.contentType),
	}, nil
}

func (m *memStore) Head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	obj, ok := m.data[key]
	if !ok {
		return nil, errors.New("NotFound")
	}
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(obj.body))),
		ContentType:   aws.String(obj.contentType),
	}, nil
}

func (m *memStore) Put(ctx context.Context, key string, body io.ReadSeeker, contentType string, contentLength int64) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.data[key] = memObj{body: b, contentType: contentType}
	return nil
}

func (m *memStore) List(ctx context.Context, prefix string, limit int32) ([]storage.Entry, error) {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	seen := map[string]storage.Entry{}
	for key, obj := range m.data {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		if rest == "" {
			continue
		}
		parts := strings.Split(rest, "/")
		if len(parts) == 1 {
			e := storage.Entry{
				Name: parts[0],
				Path: key,
				Type: "file",
				Size: int64(len(obj.body)),
			}
			seen[e.Name] = e
		} else {
			name := parts[0] + "/"
			if _, exists := seen[name]; !exists {
				seen[name] = storage.Entry{
					Name: name,
					Path: prefix + parts[0] + "/",
					Type: "dir",
				}
			}
		}
		if limit > 0 && int32(len(seen)) >= limit {
			break
		}
	}
	var entries []storage.Entry
	for _, e := range seen {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (m *memStore) GenerateChecksums(ctx context.Context, prefix string) error { return nil }
func (m *memStore) CleanupBadChecksums(ctx context.Context, prefix string) error {
	return nil
}

func TestProxyAddAndList(t *testing.T) {
	store := newMemStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))

	if err := pm.Add(context.Background(), Proxy{Name: "central", URL: "https://repo.maven.apache.org/maven2"}); err != nil {
		t.Fatalf("add proxy: %v", err)
	}
	if err := pm.Add(context.Background(), Proxy{Name: "internal", URL: "https://example.com/maven"}); err != nil {
		t.Fatalf("add proxy: %v", err)
	}

	list, err := pm.List(context.Background())
	if err != nil {
		t.Fatalf("list proxies: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(list))
	}
}

func TestProxyFetchAndCache(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/com/acme/app/1.0/app-1.0.jar" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/java-archive")
		_, _ = w.Write([]byte("JARCONTENT"))
	}))
	defer remote.Close()

	store := newMemStore()
	pm := NewProxyManager(store, zaptest.NewLogger(t))
	if err := pm.Add(context.Background(), Proxy{Name: "central", URL: remote.URL}); err != nil {
		t.Fatalf("add proxy: %v", err)
	}

	found, err := pm.FetchAndCache(context.Background(), "central/com/acme/app/1.0/app-1.0.jar")
	if err != nil {
		t.Fatalf("fetch and cache: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true")
	}
	obj, err := store.Get(context.Background(), "central/com/acme/app/1.0/app-1.0.jar")
	if err != nil {
		t.Fatalf("cached get: %v", err)
	}
	defer obj.Body.Close()
	body, _ := io.ReadAll(obj.Body)
	if string(body) != "JARCONTENT" {
		t.Fatalf("unexpected body %q", string(body))
	}
}
