package uploads

import (
	"context"
	"errors"
	"io"
	"path"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3Blobs is an S3-compatible Blobstore (AWS S3, MinIO, Cloudflare R2). Keys are
// the Store's content-addressed rel_path, optionally under a prefix (e.g. a
// community id) so a shared platform bucket separates tenants.
type s3Blobs struct {
	client *minio.Client
	bucket string
	prefix string
}

// S3Config configures an S3-compatible blobstore.
type S3Config struct {
	Endpoint     string // host[:port]; empty = AWS (s3.amazonaws.com). http:// forces insecure.
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	UsePathStyle bool   // true for MinIO/R2
	Prefix       string // object-key prefix (e.g. community id); optional
}

// NewS3Blobstore builds an S3 Blobstore. It does NOT create the bucket — the
// platform/owner provisions it; the constructor only validates connectivity
// lazily (first Put/Get).
func NewS3Blobstore(cfg S3Config) (Blobstore, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("uploads: S3 bucket is required")
	}
	endpoint := cfg.Endpoint
	secure := true
	switch {
	case strings.HasPrefix(endpoint, "https://"):
		endpoint = strings.TrimPrefix(endpoint, "https://")
	case strings.HasPrefix(endpoint, "http://"):
		endpoint = strings.TrimPrefix(endpoint, "http://")
		secure = false
	}
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	opts := &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: minio.BucketLookupAuto,
	}
	if cfg.UsePathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, err
	}
	return s3Blobs{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func (s s3Blobs) objKey(key string) string {
	if s.prefix == "" {
		return key
	}
	return path.Join(s.prefix, key)
}

func (s s3Blobs) Put(ctx context.Context, key string, r io.Reader) error {
	ok, err := s.Exists(ctx, key)
	if err == nil && ok {
		return nil // content-addressed: already present
	}
	// objectSize -1 → minio streams via multipart with unknown size.
	_, err = s.client.PutObject(ctx, s.bucket, s.objKey(key), r, -1,
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}

func (s s3Blobs) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.objKey(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	// minio.GetObject is lazy; stat now so a missing key errors here, not
	// mid-stream.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}

func (s s3Blobs) Remove(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.objKey(key), minio.RemoveObjectOptions{})
}

func (s s3Blobs) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, s.objKey(key), minio.StatObjectOptions{})
	if err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.StatusCode == 404 || resp.Code == "NoSuchKey" {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s s3Blobs) LocalPath(key string) (string, bool) { return "", false }
