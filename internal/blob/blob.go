// Package blob is luxd's view of the blob store: S3.
//
// There is no control-plane disk tier. A runner uploads through luxd (it
// never holds S3 credentials), luxd streams the body straight into S3, and
// downloads are presigned GET URLs for exactly the blobs a runner or client
// may read.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Config struct {
	Endpoint  string // empty for AWS
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// PublicEndpoint, if set, is the endpoint presigned URLs are signed for.
	PublicEndpoint string
}

type Store struct {
	s3      *s3.Client
	presign *s3.PresignClient
	tm      *transfermanager.Client
	bucket  string
}

func New(cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("blob: bucket is required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	opts := s3.Options{
		Region: cfg.Region,
		// Path-style so MinIO and custom endpoints work without DNS tricks.
		UsePathStyle: cfg.Endpoint != "",
	}
	if cfg.AccessKey != "" {
		opts.Credentials = credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	client := s3.New(opts)
	pub := client
	if cfg.PublicEndpoint != "" {
		o := opts
		o.BaseEndpoint = aws.String(cfg.PublicEndpoint)
		pub = s3.New(o)
	}
	return &Store{s3: client, presign: s3.NewPresignClient(pub), tm: transfermanager.New(client), bucket: cfg.Bucket}, nil
}

// Key is where a blob lives: scoped by tenant and Run.
func Key(tenantID, runID, blobID string) string {
	return fmt.Sprintf("tenants/%s/runs/%s/%s", url.PathEscape(tenantID), url.PathEscape(runID), url.PathEscape(blobID))
}

// Put streams body to key.
func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64, sha256hex string) error {
	// The body is a stream from the runner, not seekable: the transfer
	// manager uploads it in parts without buffering the whole blob.
	_, err := s.tm.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		Body:     body,
		Metadata: map[string]string{"sha256": sha256hex},
	})
	return err
}

// Get opens a blob for reading.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, 0, err
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

// PresignGet returns a URL valid for ttl.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)},
		s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

// Check verifies the bucket is reachable, so luxd fails at startup rather
// than on the first upload.
func (s *Store) Check(ctx context.Context) error {
	_, err := s.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return fmt.Errorf("bucket %s does not exist", s.bucket)
	}
	return err
}
