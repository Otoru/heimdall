package storage

import (
	"context"
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
	client     *s3.Client
	presign    *s3.PresignClient
	httpClient *http.Client
	bucket     string
	prefix     string
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

func (s *Store) List(ctx context.Context, prefix string, limit int32) ([]string, error) {
	p := strings.TrimPrefix(path.Clean("/"+prefix), "/")
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	if s.prefix != "" {
		p = strings.TrimPrefix(path.Join(s.prefix, p), "/")
		if p != "" && !strings.HasSuffix(p, "/") {
			p += "/"
		}
	}

	if limit <= 0 {
		limit = 100
	}

	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Prefix:    aws.String(p),
		MaxKeys:   aws.Int32(limit),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		return nil, err
	}

	var keys []string
	trimPrefix := strings.Trim(p, "/")
	trimPrefix = strings.TrimPrefix(trimPrefix, strings.Trim(s.prefix, "/"))
	trimPrefix = strings.TrimPrefix(trimPrefix, "/")

	for _, cp := range out.CommonPrefixes {
		if cp.Prefix == nil {
			continue
		}
		k := strings.TrimPrefix(*cp.Prefix, p)
		k = strings.TrimSuffix(k, "/")
		if k != "" {
			keys = append(keys, k+"/")
		}
	}
	for _, obj := range out.Contents {
		if obj.Key == nil {
			continue
		}
		if *obj.Key == p || *obj.Key == strings.TrimSuffix(p, "/") {
			continue
		}
		k := strings.TrimPrefix(*obj.Key, p)
		if strings.Contains(k, "/") {
			// deeper levels ignored because of delimiter; should not happen
			continue
		}
		if k != "" {
			keys = append(keys, k)
		}
	}

	return keys, nil
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
