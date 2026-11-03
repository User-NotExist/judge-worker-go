package pkgcache

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Fetcher reads packages from RustFS (S3 API) with a read-only key.
type S3Fetcher struct {
	client *minio.Client
	bucket string
}

func NewS3Fetcher(endpoint, bucket, region, access, secret string, transport http.RoundTripper) (*S3Fetcher, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("storage.endpoint %q: must be a URL like https://host:9000", endpoint)
	}
	cl, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(access, secret, ""),
		Secure:       u.Scheme == "https",
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
		Transport:    transport,
	})
	if err != nil {
		return nil, err
	}
	return &S3Fetcher{client: cl, bucket: bucket}, nil
}

func (f *S3Fetcher) Fetch(ctx context.Context, key, dst string) error {
	return f.client.FGetObject(ctx, f.bucket, key, dst, minio.GetObjectOptions{})
}
