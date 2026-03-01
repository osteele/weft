// Package r2 provides a client for Cloudflare R2 (S3-compatible) storage.
// It is used to retrieve job results uploaded by Vast.ai instances.
package r2

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Client wraps an S3-compatible client configured for Cloudflare R2.
type Client struct {
	s3     *s3.Client
	bucket string
}

// Config holds R2 connection settings.
type Config struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
}

// New creates a new R2 client.
func New(cfg Config) (*Client, error) {
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)

	awsCfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, "",
		)),
		config.WithRegion("auto"),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})

	return &Client{s3: s3Client, bucket: cfg.Bucket}, nil
}

// ListCompleted returns job IDs that have a .complete marker under the given prefix.
// It lists objects under prefix/jobs/ and looks for .complete files.
func (c *Client) ListCompleted(ctx context.Context, prefix string) ([]string, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	}

	var jobIDs []string
	paginator := s3.NewListObjectsV2Paginator(c.s3, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			if strings.HasSuffix(key, "/.complete") {
				// Extract job ID: prefix/jobs/<job-id>/.complete
				parts := strings.Split(key, "/")
				for i, p := range parts {
					if p == "jobs" && i+1 < len(parts) {
						jobIDs = append(jobIDs, parts[i+1])
						break
					}
				}
			}
		}
	}

	return jobIDs, nil
}

// DownloadResults downloads all files under prefix to a local directory.
func (c *Client) DownloadResults(ctx context.Context, prefix string, localDir string) error {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	}

	paginator := s3.NewListObjectsV2Paginator(c.s3, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}

		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			relPath := strings.TrimPrefix(key, prefix)
			relPath = strings.TrimPrefix(relPath, "/")
			if relPath == "" {
				continue
			}

			localPath := filepath.Join(localDir, relPath)
			if err := c.downloadObject(ctx, key, localPath); err != nil {
				return fmt.Errorf("download %s: %w", key, err)
			}
		}
	}

	return nil
}

// DeletePrefix deletes all objects under the given prefix.
func (c *Client) DeletePrefix(ctx context.Context, prefix string) error {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	}

	paginator := s3.NewListObjectsV2Paginator(c.s3, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}

		for _, obj := range page.Contents {
			_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(c.bucket),
				Key:    obj.Key,
			})
			if err != nil {
				return fmt.Errorf("delete %s: %w", aws.ToString(obj.Key), err)
			}
		}
	}

	return nil
}

// downloadObject downloads a single S3 object to a local file.
func (c *Client) downloadObject(ctx context.Context, key string, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}

	resp, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}
