package storage

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go"
)

type Options struct {
	Bucket       string
	Prefix       string
	Region       string
	Endpoint     string
	AccessKey    string
	SecretKey    string
	UsePathStyle bool
}

type Store struct {
	client     s3API
	presign    presignAPI
	httpClient *http.Client
	bucket     string
	prefix     string
}

type s3API interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

type presignAPI interface {
	PresignPutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

type Entry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // file, dir, proxy
	Size int64  `json:"size,omitempty"`
}

func New(ctx context.Context, opts Options) (*Store, error) {
	if opts.Bucket == "" {
		return nil, fmt.Errorf("bucket is required")
	}

	cfgLoaders := []func(*config.LoadOptions) error{
		config.WithRegion(opts.Region),
	}

	if opts.AccessKey != "" && opts.SecretKey != "" {
		cfgLoaders = append(cfgLoaders, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, "")))
	}

	if opts.Endpoint != "" {
		resolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, _ ...interface{}) (aws.Endpoint, error) {
			if service == s3.ServiceID {
				return aws.Endpoint{
					URL:               opts.Endpoint,
					SigningRegion:     opts.Region,
					HostnameImmutable: true,
				}, nil
			}
			return aws.Endpoint{}, &aws.EndpointNotFoundError{}
		})
		cfgLoaders = append(cfgLoaders, config.WithEndpointResolverWithOptions(resolver))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, cfgLoaders...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = opts.UsePathStyle
		o.DisableLogOutputChecksumValidationSkipped = true
	})

	return &Store{
		client:     client,
		presign:    s3.NewPresignClient(client),
		httpClient: http.DefaultClient,
		bucket:     opts.Bucket,
		prefix:     strings.Trim(opts.Prefix, "/"),
	}, nil
}

func (s *Store) key(raw string) string {
	if s.prefix == "" {
		return raw
	}
	return strings.TrimPrefix(path.Join(s.prefix, raw), "/")
}

func (s *Store) cleanKey(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty key")
	}

	cleaned := strings.TrimPrefix(path.Clean("/"+raw), "/")
	if cleaned == "" || cleaned == "." {
		return "", fmt.Errorf("invalid key")
	}

	return s.key(cleaned), nil
}

func (s *Store) Get(ctx context.Context, key string) (*s3.GetObjectOutput, error) {
	k, err := s.cleanKey(key)
	if err != nil {
		return nil, err
	}
	return s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(k),
	})
}

func (s *Store) Head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	k, err := s.cleanKey(key)
	if err != nil {
		return nil, err
	}
	return s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(k),
	})
}

func (s *Store) Put(ctx context.Context, key string, body io.ReadSeeker, contentType string, contentLength int64) error {
	k, err := s.cleanKey(key)
	if err != nil {
		return err
	}

	putInput := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(k),
	}
	if contentType != "" {
		putInput.ContentType = aws.String(contentType)
	}
	if contentLength >= 0 {
		putInput.ContentLength = aws.Int64(contentLength)
	}

	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek body: %w", err)
	}

	psReq, err := s.presign.PresignPutObject(ctx, putInput)
	if err != nil {
		return fmt.Errorf("presign put: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, psReq.URL, io.NopCloser(body))
	if err != nil {
		return fmt.Errorf("build put request: %w", err)
	}
	req.ContentLength = contentLength
	for k, vals := range psReq.SignedHeader {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		slurp, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(slurp)))
	}

	return nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	k, err := s.cleanKey(key)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(k),
	})
	return err
}

func (s *Store) putAbsolute(ctx context.Context, key string, body io.ReadSeeker, contentType string, contentLength int64) error {
	putInput := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	if contentType != "" {
		putInput.ContentType = aws.String(contentType)
	}
	if contentLength >= 0 {
		putInput.ContentLength = aws.Int64(contentLength)
	}

	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek body: %w", err)
	}

	psReq, err := s.presign.PresignPutObject(ctx, putInput)
	if err != nil {
		return fmt.Errorf("presign put: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, psReq.URL, io.NopCloser(body))
	if err != nil {
		return fmt.Errorf("build put request: %w", err)
	}
	req.ContentLength = contentLength
	for k, vals := range psReq.SignedHeader {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		slurp, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(slurp)))
	}
	return nil
}

func buildListPrefix(prefix, storePrefix string) string {
	p := strings.TrimPrefix(path.Clean("/"+prefix), "/")
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	if storePrefix != "" {
		p = strings.TrimPrefix(path.Join(storePrefix, p), "/")
		if p != "" && !strings.HasSuffix(p, "/") {
			p += "/"
		}
	}
	return p
}

func appendCommonPrefixes(keys []Entry, prefixes []types.CommonPrefix, p, basePath string) []Entry {
	for _, cp := range prefixes {
		if cp.Prefix == nil {
			continue
		}
		k := strings.TrimSuffix(strings.TrimPrefix(*cp.Prefix, p), "/")
		if k != "" {
			keys = append(keys, Entry{Name: k + "/", Path: path.Join(basePath, k) + "/", Type: "dir"})
		}
	}
	return keys
}

func objectSize(obj types.Object) int64 {
	if obj.Size != nil {
		return *obj.Size
	}
	return 0
}

func appendObjects(keys []Entry, objects []types.Object, p, basePath string) []Entry {
	for _, obj := range objects {
		if obj.Key == nil {
			continue
		}
		if *obj.Key == p || *obj.Key == strings.TrimSuffix(p, "/") {
			continue
		}
		k := strings.TrimPrefix(*obj.Key, p)
		if strings.Contains(k, "/") || k == "" {
			continue
		}
		keys = append(keys, Entry{Name: k, Path: path.Join(basePath, k), Type: "file", Size: objectSize(obj)})
	}
	return keys
}

func isTruncated(out *s3.ListObjectsV2Output) bool {
	return out.IsTruncated != nil && *out.IsTruncated && out.NextContinuationToken != nil
}

func (s *Store) List(ctx context.Context, prefix string, limit int32) ([]Entry, error) {
	p := buildListPrefix(prefix, s.prefix)
	if limit <= 0 {
		limit = 100
	}
	basePath := strings.TrimSuffix(p, "/")
	var (
		keys  []Entry
		token *string
	)
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(p),
			MaxKeys:           aws.Int32(limit - int32(len(keys))),
			Delimiter:         aws.String("/"),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		keys = appendCommonPrefixes(keys, out.CommonPrefixes, p, basePath)
		keys = appendObjects(keys, out.Contents, p, basePath)
		if int32(len(keys)) >= limit || !isTruncated(out) {
			break
		}
		token = out.NextContinuationToken
	}
	if int32(len(keys)) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

func normalizeScanPrefix(prefix, storePrefix string) string {
	p := strings.TrimPrefix(path.Clean("/"+prefix), "/")
	if storePrefix != "" {
		p = path.Join(storePrefix, p)
	}
	return strings.TrimPrefix(p, "/")
}

func isChecksumFile(key string) bool {
	return strings.HasSuffix(key, "/") || strings.HasSuffix(key, ".sha1") || strings.HasSuffix(key, ".md5")
}

func (s *Store) needsChecksum(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return false, nil
	}
	if IsNotFound(err) {
		return true, nil
	}
	return false, err
}

func (s *Store) deleteIfBadChecksum(ctx context.Context, key string, badSuffixes []string) {
	for _, suf := range badSuffixes {
		if strings.HasSuffix(key, suf) {
			_, _ = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(key),
			})
			return
		}
	}
}

func (s *Store) GenerateChecksums(ctx context.Context, prefix string) error {
	p := normalizeScanPrefix(prefix, s.prefix)
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(p),
			ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		for _, obj := range out.Contents {
			if obj.Key == nil || isChecksumFile(*obj.Key) {
				continue
			}
			if err := s.ensureChecksums(ctx, *obj.Key); err != nil {
				return err
			}
		}
		if !isTruncated(out) {
			break
		}
		token = out.NextContinuationToken
	}
	return nil
}

func (s *Store) CleanupBadChecksums(ctx context.Context, prefix string) error {
	p := normalizeScanPrefix(prefix, s.prefix)
	badSuffixes := []string{".sha1.sha1", ".sha1.md5", ".md5.sha1", ".md5.md5"}
	var token *string
	for {
		out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(p),
			ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		for _, obj := range out.Contents {
			if obj.Key != nil {
				s.deleteIfBadChecksum(ctx, *obj.Key, badSuffixes)
			}
		}
		if !isTruncated(out) {
			break
		}
		token = out.NextContinuationToken
	}
	return nil
}

func (s *Store) ensureChecksums(ctx context.Context, key string) error {
	needsSha1, err := s.needsChecksum(ctx, key+".sha1")
	if err != nil {
		return err
	}
	needsMd5, err := s.needsChecksum(ctx, key+".md5")
	if err != nil {
		return err
	}
	if !needsSha1 && !needsMd5 {
		return nil
	}

	obj, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer obj.Body.Close()

	sha1h := sha1.New()
	md5h := md5.New()
	if _, err := io.Copy(io.MultiWriter(sha1h, md5h), obj.Body); err != nil {
		return err
	}

	if needsSha1 {
		sum := hex.EncodeToString(sha1h.Sum(nil))
		if err := s.putAbsolute(ctx, key+".sha1", strings.NewReader(sum), "text/plain", int64(len(sum))); err != nil {
			return err
		}
	}

	if needsMd5 {
		sum := hex.EncodeToString(md5h.Sum(nil))
		if err := s.putAbsolute(ctx, key+".md5", strings.NewReader(sum), "text/plain", int64(len(sum))); err != nil {
			return err
		}
	}

	return nil
}

func IsNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "NotFoundException":
			return true
		}
	}

	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}

	if err != nil && strings.Contains(err.Error(), "NotFound") {
		return true
	}

	return false
}
