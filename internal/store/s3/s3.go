// Package s3 is a store backend for Amazon S3 and S3-compatible services
// (DigitalOcean Spaces, MinIO, R2). PutIfAbsent uses conditional writes
// (If-None-Match: *); run `kura probe` to confirm a provider honors them.
//
// URL form: s3://bucket/prefix?region=us-east-2&endpoint=https://...&path_style=true
// &profile=NAME (shared AWS config profile) or &creds=NAME (static key stored
// with `kura s3-creds set NAME`).
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/digital-shokunin/kura/internal/store"
)

// CredentialLookup resolves a named static credential (the creds= URL
// parameter). It is set by the caller so this package stays free of
// platform keychain code.
var CredentialLookup func(name string) (accessKeyID, secretAccessKey string, err error)

func init() {
	store.Register("s3", func(ctx context.Context, u *url.URL) (store.Store, error) { return Open(ctx, u) })
}

// Store is an S3 bucket plus key prefix.
type Store struct {
	client *s3.Client
	bucket string
	prefix string
}

// Open configures a client from the URL.
func Open(ctx context.Context, u *url.URL) (*Store, error) {
	if u.Host == "" {
		return nil, errors.New("s3: url needs a bucket: s3://bucket/prefix")
	}
	q := u.Query()
	opts := []func(*config.LoadOptions) error{
		// The SDK's default CRC32 checksums (since 2025) break many
		// S3-compatible services.
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	if r := q.Get("region"); r != "" {
		opts = append(opts, config.WithRegion(r))
	}
	if p := q.Get("profile"); p != "" {
		opts = append(opts, config.WithSharedConfigProfile(p))
	}
	if name := q.Get("creds"); name != "" {
		if CredentialLookup == nil {
			return nil, errors.New("s3: creds= is not supported on this platform")
		}
		id, secret, err := CredentialLookup(name)
		if err != nil {
			return nil, fmt.Errorf("s3: loading credentials %q: %w", name, err)
		}
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(id, secret, "")))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("s3: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("s3: no region; add ?region=... to the url")
	}
	endpoint := q.Get("endpoint")
	pathStyle, _ := strconv.ParseBool(q.Get("path_style"))
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = pathStyle
	})
	prefix := strings.Trim(u.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &Store{client: client, bucket: u.Host, prefix: prefix}, nil
}

func (s *Store) key(k string) *string { return aws.String(s.prefix + k) }

func statusCode(err error) int {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// forbiddenHint explains the most common cause of 403s on reads: without
// s3:ListBucket, S3 reports missing objects as 403 instead of 404, which
// breaks kura's probing for the newest manifest.
func forbiddenHint(err error) error {
	if statusCode(err) == http.StatusForbidden {
		return fmt.Errorf("%w (403 Forbidden: the credentials need s3:GetObject, s3:PutObject, "+
			"s3:DeleteObject and s3:ListBucket; without ListBucket missing objects look forbidden)", err)
	}
	return err
}

func isNotFound(err error) bool {
	switch apiCode(err) {
	case "NoSuchKey", "NotFound":
		return true
	}
	return statusCode(err) == http.StatusNotFound
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: s.key(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, forbiddenHint(err)
	}
	return out.Body, nil
}

// seekable returns r as a ReadSeeker, buffering it if necessary, so the SDK
// can sign and retry the request.
func seekable(r io.Reader) (io.ReadSeeker, error) {
	if rs, ok := r.(io.ReadSeeker); ok {
		return rs, nil
	}
	b, err := io.ReadAll(r)
	return bytes.NewReader(b), err
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	body, err := seekable(r)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: s.key(key), Body: body, ContentLength: aws.Int64(size),
	})
	return err
}

func (s *Store) PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) error {
	body, err := seekable(r)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &s.bucket, Key: s.key(key), Body: body, ContentLength: aws.Int64(size),
			IfNoneMatch: aws.String("*"),
		})
		switch {
		case err == nil:
			return nil
		case statusCode(err) == http.StatusPreconditionFailed || apiCode(err) == "PreconditionFailed":
			return store.ErrExists
		case statusCode(err) == http.StatusConflict && attempt < 5:
			// ConditionalRequestConflict: a concurrent conditional write to
			// the same key is in flight. Retry; one of us will get 412.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(100<<attempt) * time.Millisecond):
			}
		default:
			return err
		}
	}
}

func (s *Store) Head(ctx context.Context, key string) (store.Info, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: s.key(key)})
	if err != nil {
		if isNotFound(err) {
			return store.Info{}, store.ErrNotFound
		}
		return store.Info{}, forbiddenHint(err)
	}
	info := store.Info{Size: aws.ToInt64(out.ContentLength)}
	if out.LastModified != nil {
		info.ModTime = *out.LastModified
	}
	return info, nil
}

func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: s.key(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			keys = append(keys, strings.TrimPrefix(aws.ToString(o.Key), s.prefix))
		}
	}
	return keys, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: s.key(key)})
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}
