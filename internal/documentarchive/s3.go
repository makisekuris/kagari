package documentarchive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	defaultS3Region = "us-east-1"
	maxPresignTTL   = 7 * 24 * time.Hour
)

// S3 stores archive objects in an S3-compatible service such as RustFS.
type S3 struct {
	bucket    string
	client    *s3.Client
	presigner *s3.PresignClient
}

// NewS3 creates a path-style S3 client for a custom endpoint.
func NewS3(endpoint, bucket, region, accessKeyID, secretAccessKey string) (*S3, error) {
	endpoint = strings.TrimSpace(endpoint)
	bucket = strings.TrimSpace(bucket)
	region = strings.TrimSpace(region)
	if region == "" {
		region = defaultS3Region
	}
	if bucket == "" {
		return nil, errors.New("archive bucket is required")
	}
	if strings.TrimSpace(accessKeyID) == "" || strings.TrimSpace(secretAccessKey) == "" {
		return nil, errors.New("archive access key ID and secret access key are required")
	}
	if err := validateS3Endpoint(endpoint); err != nil {
		return nil, err
	}

	awsConfig := aws.Config{
		Region:      region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")),
		// Some S3-compatible servers do not support optional flexible checksums.
		// Required checksums remain enabled for operations that need them.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(endpoint)
		options.UsePathStyle = true
	})

	return &S3{
		bucket:    bucket,
		client:    client,
		presigner: s3.NewPresignClient(client),
	}, nil
}

func validateS3Endpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (strings.ToLower(parsed.Scheme) != "http" && strings.ToLower(parsed.Scheme) != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("archive endpoint must be an HTTP(S) URL without credentials, query or fragment")
	}
	return nil
}

func (s *S3) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, mediaType string) error {
	if key == "" {
		return errors.New("archive object key is required")
	}
	if body == nil {
		return errors.New("archive object body is required")
	}
	if size < 0 {
		return errors.New("archive object size cannot be negative")
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(mediaType),
	})
	if err != nil {
		return fmt.Errorf("put archive object: %w", err)
	}
	return nil
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == "" {
		return nil, errors.New("archive object key is required")
	}
	object, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get archive object: %w", err)
	}
	return object.Body, nil
}

func (s *S3) DownloadURL(ctx context.Context, key, name string, ttl time.Duration) (string, error) {
	if key == "" {
		return "", errors.New("archive object key is required")
	}
	if ttl < time.Second || ttl > maxPresignTTL {
		return "", errors.New("archive download URL TTL must be between 1 second and 7 days")
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": safeDownloadFilename(name)})
	if disposition == "" {
		return "", errors.New("archive download filename is invalid")
	}
	presigned, err := s.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(s.bucket),
		Key:                        aws.String(key),
		ResponseContentDisposition: aws.String(disposition),
	}, func(options *s3.PresignOptions) {
		options.Expires = ttl
	})
	if err != nil {
		return "", fmt.Errorf("presign archive download: %w", err)
	}
	return presigned.URL, nil
}

func safeDownloadFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(name)
	name = strings.ToValidUTF8(name, "�")
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "download"
	}
	return name
}
