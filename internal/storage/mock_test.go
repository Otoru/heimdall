package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// ---- mocks ----

type mockS3Client struct {
	getOut    *s3.GetObjectOutput
	getErr    error
	headOut   *s3.HeadObjectOutput
	headErr   error
	deleteErr error
	listOut   *s3.ListObjectsV2Output
	listErr   error
}

func (m *mockS3Client) GetObject(_ context.Context, _ *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return m.getOut, m.getErr
}
func (m *mockS3Client) HeadObject(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return m.headOut, m.headErr
}
func (m *mockS3Client) PutObject(_ context.Context, _ *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return &s3.PutObjectOutput{}, nil
}
func (m *mockS3Client) DeleteObject(_ context.Context, _ *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return &s3.DeleteObjectOutput{}, m.deleteErr
}
func (m *mockS3Client) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	if m.listOut != nil {
		return m.listOut, nil
	}
	return &s3.ListObjectsV2Output{}, nil
}

type mockPresigner struct {
	url string
	err error
}

func (m *mockPresigner) PresignPutObject(_ context.Context, _ *s3.PutObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &v4.PresignedHTTPRequest{URL: m.url}, nil
}

type pagedMockS3Client struct {
	mockS3Client
	calls int
	pages []*s3.ListObjectsV2Output
}

func (m *pagedMockS3Client) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if m.calls < len(m.pages) {
		out := m.pages[m.calls]
		m.calls++
		return out, nil
	}
	return &s3.ListObjectsV2Output{}, nil
}

func newMockStore(client *mockS3Client) *Store {
	return &Store{client: client, presign: &mockPresigner{}, httpClient: http.DefaultClient, bucket: "test-bucket"}
}

func newMockStoreWithUpload(client *mockS3Client, uploadURL string) *Store {
	return &Store{client: client, presign: &mockPresigner{url: uploadURL}, httpClient: http.DefaultClient, bucket: "test-bucket"}
}

// ---- tests ----

func TestNewBucketRequired(t *testing.T) {
	_, err := New(context.Background(), Options{})
	if err == nil {
		t.Fatal("expected error for empty bucket")
	}
}

func TestKeyWithPrefix(t *testing.T) {
	s := &Store{prefix: "releases"}
	if got := s.key("file.jar"); got != "releases/file.jar" {
		t.Fatalf("expected releases/file.jar, got %s", got)
	}
}

func TestGet(t *testing.T) {
	content := "artifact-bytes"
	client := &mockS3Client{
		getOut: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader(content)),
			ContentLength: aws.Int64(int64(len(content))),
		},
	}
	out, err := newMockStore(client).Get(context.Background(), "group/artifact.jar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer out.Body.Close()
	body, _ := io.ReadAll(out.Body)
	if string(body) != content {
		t.Fatalf("expected %q, got %q", content, body)
	}
}

func TestGetError(t *testing.T) {
	_, err := newMockStore(&mockS3Client{getErr: errors.New("s3 error")}).Get(context.Background(), "key.jar")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGetEmptyKey(t *testing.T) {
	_, err := newMockStore(&mockS3Client{}).Get(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestHead(t *testing.T) {
	client := &mockS3Client{headOut: &s3.HeadObjectOutput{ContentLength: aws.Int64(42)}}
	out, err := newMockStore(client).Head(context.Background(), "artifact.jar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if aws.ToInt64(out.ContentLength) != 42 {
		t.Fatalf("expected content length 42")
	}
}

func TestHeadError(t *testing.T) {
	_, err := newMockStore(&mockS3Client{headErr: errors.New("s3 error")}).Head(context.Background(), "artifact.jar")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHeadEmptyKey(t *testing.T) {
	_, err := newMockStore(&mockS3Client{}).Head(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestDelete(t *testing.T) {
	if err := newMockStore(&mockS3Client{}).Delete(context.Background(), "artifact.jar"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteError(t *testing.T) {
	if err := newMockStore(&mockS3Client{deleteErr: errors.New("err")}).Delete(context.Background(), "artifact.jar"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteEmptyKey(t *testing.T) {
	if err := newMockStore(&mockS3Client{}).Delete(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestListFilesAndDirs(t *testing.T) {
	client := &mockS3Client{
		listOut: &s3.ListObjectsV2Output{
			CommonPrefixes: []types.CommonPrefix{{Prefix: aws.String("myprefix/subdir/")}},
			Contents:       []types.Object{{Key: aws.String("myprefix/file.jar"), Size: aws.Int64(100)}},
		},
	}
	entries, err := newMockStore(client).List(context.Background(), "myprefix", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	var gotDir, gotFile bool
	for _, e := range entries {
		if e.Type == "dir" && e.Name == "subdir/" {
			gotDir = true
		}
		if e.Type == "file" && e.Name == "file.jar" && e.Size == 100 {
			gotFile = true
		}
	}
	if !gotDir || !gotFile {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestListWithStorePrefix(t *testing.T) {
	client := &mockS3Client{
		listOut: &s3.ListObjectsV2Output{
			Contents: []types.Object{{Key: aws.String("releases/artifact.jar"), Size: aws.Int64(50)}},
		},
	}
	store := &Store{client: client, presign: &mockPresigner{}, httpClient: http.DefaultClient, bucket: "b", prefix: "releases"}
	entries, err := store.List(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

func TestListDefaultLimit(t *testing.T) {
	_, err := newMockStore(&mockS3Client{}).List(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestListError(t *testing.T) {
	_, err := newMockStore(&mockS3Client{listErr: errors.New("list error")}).List(context.Background(), "", 10)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListPagination(t *testing.T) {
	token := "next-token"
	client := &pagedMockS3Client{
		pages: []*s3.ListObjectsV2Output{
			{
				Contents:              []types.Object{{Key: aws.String("prefix/a.jar"), Size: aws.Int64(1)}},
				IsTruncated:           aws.Bool(true),
				NextContinuationToken: aws.String(token),
			},
			{
				Contents: []types.Object{{Key: aws.String("prefix/b.jar"), Size: aws.Int64(2)}},
			},
		},
	}
	store := &Store{client: client, presign: &mockPresigner{}, httpClient: http.DefaultClient, bucket: "b"}
	entries, err := store.List(context.Background(), "prefix", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
}

func TestListLimitTruncates(t *testing.T) {
	client := &mockS3Client{
		listOut: &s3.ListObjectsV2Output{
			Contents: []types.Object{
				{Key: aws.String("prefix/a.jar"), Size: aws.Int64(1)},
				{Key: aws.String("prefix/b.jar"), Size: aws.Int64(2)},
				{Key: aws.String("prefix/c.jar"), Size: aws.Int64(3)},
			},
		},
	}
	entries, err := newMockStore(client).List(context.Background(), "prefix", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected limit of 2 entries, got %d", len(entries))
	}
}

func TestGenerateChecksumsEmpty(t *testing.T) {
	if err := newMockStore(&mockS3Client{}).GenerateChecksums(context.Background(), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerateChecksumsError(t *testing.T) {
	if err := newMockStore(&mockS3Client{listErr: errors.New("err")}).GenerateChecksums(context.Background(), ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestGenerateChecksumsAlreadyPresent(t *testing.T) {
	client := &mockS3Client{
		listOut: &s3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String("file.jar")}}},
		headOut: &s3.HeadObjectOutput{},
	}
	if err := newMockStore(client).GenerateChecksums(context.Background(), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerateChecksumsWithPrefix(t *testing.T) {
	store := &Store{client: &mockS3Client{}, presign: &mockPresigner{}, httpClient: http.DefaultClient, bucket: "b", prefix: "releases"}
	if err := store.GenerateChecksums(context.Background(), "snapshots"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureChecksumsGenerates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := &mockS3Client{
		headErr: &smithy.GenericAPIError{Code: "NotFound"},
		getOut:  &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("data"))},
	}
	store := newMockStoreWithUpload(client, ts.URL+"/upload")
	if err := store.ensureChecksums(context.Background(), "file.jar"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCleanupBadChecksumsDetectsAndDeletes(t *testing.T) {
	client := &mockS3Client{
		listOut: &s3.ListObjectsV2Output{
			Contents: []types.Object{
				{Key: aws.String("file.jar.sha1.sha1")},
				{Key: aws.String("file.jar.md5.md5")},
				{Key: aws.String("file.jar")},
			},
		},
	}
	if err := newMockStore(client).CleanupBadChecksums(context.Background(), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCleanupBadChecksumsEmpty(t *testing.T) {
	if err := newMockStore(&mockS3Client{}).CleanupBadChecksums(context.Background(), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCleanupBadChecksumsError(t *testing.T) {
	if err := newMockStore(&mockS3Client{listErr: errors.New("err")}).CleanupBadChecksums(context.Background(), ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanupBadChecksumsPagination(t *testing.T) {
	token := "next"
	client := &pagedMockS3Client{
		pages: []*s3.ListObjectsV2Output{
			{
				Contents:              []types.Object{{Key: aws.String("a.sha1.sha1")}},
				IsTruncated:           aws.Bool(true),
				NextContinuationToken: aws.String(token),
			},
			{Contents: []types.Object{{Key: aws.String("b.md5.md5")}}},
		},
	}
	store := &Store{client: client, presign: &mockPresigner{}, httpClient: http.DefaultClient, bucket: "b"}
	if err := store.CleanupBadChecksums(context.Background(), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIsNotFoundNoSuchKey(t *testing.T) {
	if !IsNotFound(&types.NoSuchKey{}) {
		t.Fatal("expected IsNotFound for NoSuchKey")
	}
}

func TestIsNotFoundNil(t *testing.T) {
	if IsNotFound(nil) {
		t.Fatal("nil should not be IsNotFound")
	}
}

func TestPutPresignError(t *testing.T) {
	store := &Store{
		client:     &mockS3Client{},
		presign:    &mockPresigner{err: errors.New("presign failed")},
		httpClient: http.DefaultClient,
		bucket:     "test-bucket",
	}
	if err := store.Put(context.Background(), "key.jar", strings.NewReader("data"), "", 4); err == nil {
		t.Fatal("expected error from presign failure")
	}
}

func TestPutSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	content := "artifact-content"
	store := newMockStoreWithUpload(&mockS3Client{}, ts.URL+"/upload")
	if err := store.Put(context.Background(), "key.jar", strings.NewReader(content), "application/java-archive", int64(len(content))); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPutUploadStatusError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer ts.Close()

	store := newMockStoreWithUpload(&mockS3Client{}, ts.URL+"/upload")
	if err := store.Put(context.Background(), "key.jar", strings.NewReader("data"), "", 4); err == nil {
		t.Fatal("expected error for upload failure")
	}
}

func TestIsNotFoundAPIErrorCode(t *testing.T) {
	if !IsNotFound(&smithy.GenericAPIError{Code: "NotFound"}) {
		t.Fatal("expected IsNotFound for APIError NotFound")
	}
}

func TestIsNotFoundAPIErrorNoSuchKeyCode(t *testing.T) {
	if !IsNotFound(&smithy.GenericAPIError{Code: "NoSuchKey"}) {
		t.Fatal("expected IsNotFound for APIError NoSuchKey")
	}
}

func TestIsNotFoundStringContains(t *testing.T) {
	if !IsNotFound(errors.New("key NotFound in bucket")) {
		t.Fatal("expected IsNotFound for error string containing NotFound")
	}
}

func TestIsNotFoundOtherError(t *testing.T) {
	if IsNotFound(errors.New("connection refused")) {
		t.Fatal("connection refused should not be NotFound")
	}
}

func TestCleanKeySlash(t *testing.T) {
	store := &Store{client: &mockS3Client{}, presign: &mockPresigner{}, httpClient: http.DefaultClient, bucket: "b"}
	_, err := store.Get(context.Background(), "/")
	if err == nil {
		t.Fatal("expected error for slash key")
	}
}

func TestEnsureChecksumsUploadFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := &mockS3Client{
		headErr: &smithy.GenericAPIError{Code: "NotFound"},
		getOut:  &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("data"))},
	}
	store := newMockStoreWithUpload(client, ts.URL+"/upload")
	if err := store.ensureChecksums(context.Background(), "file.jar"); err == nil {
		t.Fatal("expected error for upload failure")
	}
}
