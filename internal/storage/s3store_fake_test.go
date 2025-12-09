package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type fakeObj struct {
    body        []byte
    contentType string
}

type fakeS3 struct {
    objects map[string]fakeObj
}

func newFakeS3() *fakeS3 {
    return &fakeS3{objects: make(map[string]fakeObj)}
}

func (f *fakeS3) GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
    key := aws.ToString(params.Key)
    obj, ok := f.objects[key]
    if !ok {
        return nil, notFoundErr()
    }
    return &s3.GetObjectOutput{
        Body:          io.NopCloser(bytes.NewReader(obj.body)),
        ContentLength: aws.Int64(int64(len(obj.body))),
        ContentType:   aws.String(obj.contentType),
    }, nil
}

func (f *fakeS3) HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
    key := aws.ToString(params.Key)
    obj, ok := f.objects[key]
    if !ok {
        return nil, notFoundErr()
    }
    return &s3.HeadObjectOutput{
        ContentLength: aws.Int64(int64(len(obj.body))),
        ContentType:   aws.String(obj.contentType),
    }, nil
}

func (f *fakeS3) PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
    key := aws.ToString(params.Key)
    data, err := io.ReadAll(params.Body)
    if err != nil {
        return nil, err
    }
    ct := aws.ToString(params.ContentType)
    f.objects[key] = fakeObj{body: data, contentType: ct}
    return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
    key := aws.ToString(params.Key)
    delete(f.objects, key)
    return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
    prefix := aws.ToString(params.Prefix)
    delim := aws.ToString(params.Delimiter)
    max := int(aws.ToInt32(params.MaxKeys))
    if max <= 0 {
        max = 1000
    }

    commons := map[string]struct{}{}
    contents := []types.Object{}
    count := 0
    for key, obj := range f.objects {
        if !strings.HasPrefix(key, prefix) {
            continue
        }
        rest := strings.TrimPrefix(key, prefix)
        if delim != "" {
            parts := strings.Split(rest, delim)
            if len(parts) > 1 {
                commons[prefix+parts[0]+delim] = struct{}{}
                continue
            }
        }
        contents = append(contents, types.Object{Key: aws.String(key), Size: aws.Int64(int64(len(obj.body)))})
        count++
        if count >= max {
            break
        }
    }
    var cps []types.CommonPrefix
    for k := range commons {
        cp := k
        cps = append(cps, types.CommonPrefix{Prefix: aws.String(cp)})
    }
    return &s3.ListObjectsV2Output{Contents: contents, CommonPrefixes: cps}, nil
}

type fakePresign struct{}

func (fakePresign) PresignPutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	u := &url.URL{Scheme: "http", Host: "fake", Path: aws.ToString(params.Key)}
	return &v4.PresignedHTTPRequest{URL: u.String(), Method: http.MethodPut, SignedHeader: http.Header{}}, nil
}

type fakeTransport struct {
    store *fakeS3
}

func (t fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
    key := strings.TrimPrefix(req.URL.Path, "/")
    data, err := io.ReadAll(req.Body)
    if err != nil {
        return nil, err
    }
    ct := req.Header.Get("Content-Type")
    t.store.objects[key] = fakeObj{body: data, contentType: ct}
    return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func notFoundErr() error {
    return &smithy.GenericAPIError{Code: "NotFound", Message: "not found"}
}
