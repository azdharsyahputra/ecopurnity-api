package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Config struct {
	Endpoint        string
	PublicEndpoint  string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	PathStyle       bool
}

type Store struct {
	bucket  string
	client  *s3.Client
	presign *s3.PresignClient
}

func New(c Config) *Store {
	opts := func(endpoint string) *s3.Client {
		return s3.New(s3.Options{
			BaseEndpoint: aws.String(endpoint),
			Region:       c.Region,
			Credentials:  credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, ""),
			UsePathStyle: c.PathStyle,

			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		})
	}
	public := c.PublicEndpoint
	if public == "" {
		public = c.Endpoint
	}
	return &Store{bucket: c.Bucket, client: opts(c.Endpoint), presign: s3.NewPresignClient(opts(public))}
}

func (s *Store) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, http.Header, error) {
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), ContentType: aws.String(contentType), ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", nil, err
	}
	h := http.Header{}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", fmt.Sprint(size))
	return req.URL, h, nil
}

func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

var ErrNotFound = errors.New("object not found")

type Object struct {
	Size int64
	Head []byte
}

func (s *Store) Inspect(ctx context.Context, key string) (Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Range: aws.String("bytes=0-511")})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) || isStatus(err, http.StatusNotFound) {
			return Object{}, ErrNotFound
		}
		return Object{}, err
	}
	defer out.Body.Close()
	head, err := io.ReadAll(io.LimitReader(out.Body, 512))
	if err != nil {
		return Object{}, err
	}
	size := aws.ToInt64(out.ContentLength)
	if out.ContentRange != nil {
		var a, b, total int64
		if _, err := fmt.Sscanf(*out.ContentRange, "bytes %d-%d/%d", &a, &b, &total); err == nil {
			size = total
		}
	}
	return Object{Size: size, Head: head}, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

func (s *Store) EnsureBucket(ctx context.Context) error {
	if err := s.Ping(ctx); err == nil {
		return nil
	}
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(s.bucket)})
	return err
}

func (s *Store) SetCORS(ctx context.Context, origins []string) error {
	_, err := s.client.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String(s.bucket),
		CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{
			AllowedOrigins: origins,
			AllowedMethods: []string{"PUT", "GET", "HEAD"},
			AllowedHeaders: []string{"content-type"},
			ExposeHeaders:  []string{"etag"},
			MaxAgeSeconds:  aws.Int32(3600),
		}}},
	})
	return err
}

func (s *Store) Ping(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	return err
}

func isStatus(err error, code int) bool {
	var re interface{ HTTPStatusCode() int }
	return errors.As(err, &re) && re.HTTPStatusCode() == code
}
