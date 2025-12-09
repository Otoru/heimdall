package storage

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
)

func newTestStore(prefix string) *Store {
	fs := newFakeS3()
	return &Store{
		client:     fs,
		presign:    fakePresign{},
		httpClient: &http.Client{Transport: fakeTransport{store: fs}},
		bucket:     "bucket",
		prefix:     strings.Trim(prefix, "/"),
	}
}

func TestStorePutAndList(t *testing.T) {
	store := newTestStore("releases")
	body := bytes.NewReader([]byte("data"))
	if err := store.Put(context.Background(), "com/acme/app/1.0/app-1.0.jar", body, "application/java-archive", int64(body.Len())); err != nil {
		t.Fatalf("put: %v", err)
	}

	entries, err := store.List(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "com/" || entries[0].Type != "dir" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestGenerateChecksums(t *testing.T) {
	store := newTestStore("")
	store.client.(*fakeS3).objects["artifact.jar"] = fakeObj{body: []byte("hello"), contentType: "application/java-archive"}

	if err := store.GenerateChecksums(context.Background(), ""); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, ok := store.client.(*fakeS3).objects["artifact.jar.sha1"]; !ok {
		t.Fatalf("missing sha1")
	}
	if _, ok := store.client.(*fakeS3).objects["artifact.jar.md5"]; !ok {
		t.Fatalf("missing md5")
	}
}

func TestCleanupBadChecksums(t *testing.T) {
	store := newTestStore("")
	fs := store.client.(*fakeS3)
	fs.objects["file.jar"] = fakeObj{body: []byte("data")}
	fs.objects["file.jar.sha1"] = fakeObj{body: []byte("good")}
	fs.objects["file.jar.sha1.sha1"] = fakeObj{body: []byte("bad")}

	if err := store.CleanupBadChecksums(context.Background(), ""); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, ok := fs.objects["file.jar.sha1.sha1"]; ok {
		t.Fatalf("expected bad checksum removed")
	}
}
