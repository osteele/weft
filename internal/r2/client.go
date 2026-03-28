// Package r2 provides a client for Cloudflare R2 (S3-compatible) storage.
// It is used to retrieve job results uploaded by Vast.ai instances.
package r2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Client wraps an S3-compatible client configured for Cloudflare R2.
type Client struct {
	s3       *s3.Client
	bucket   string
	endpoint string
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

	return &Client{s3: s3Client, bucket: cfg.Bucket, endpoint: endpoint}, nil
}

// JobMarkers holds the results of a single-pass scan for job marker files.
type JobMarkers struct {
	Completed     []string
	Started       []string
	completedKeys map[int64]map[string]struct{}
	startedKeys   map[int64]map[string]struct{}
	processedKeys map[int64]map[string]struct{}
}

// ListJobMarkers scans for both .complete and .started markers in a single pass.
func (c *Client) ListJobMarkers(ctx context.Context, prefix string) (*JobMarkers, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	}

	result := &JobMarkers{
		completedKeys: make(map[int64]map[string]struct{}),
		startedKeys:   make(map[int64]map[string]struct{}),
		processedKeys: make(map[int64]map[string]struct{}),
	}
	paginator := s3.NewListObjectsV2Paginator(c.s3, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			var (
				target     *[]string
				targetKeys map[int64]map[string]struct{}
			)
			switch {
			case strings.HasSuffix(key, "/.complete"):
				target = &result.Completed
				targetKeys = result.completedKeys
			case strings.HasSuffix(key, "/.started"):
				target = &result.Started
				targetKeys = result.startedKeys
			case strings.HasSuffix(key, "/.processed"):
				targetKeys = result.processedKeys
			default:
				continue
			}
			// Extract job ID: prefix/jobs/<job-id>/<marker>
			parts := strings.Split(key, "/")
			for i, p := range parts {
				if p == "jobs" && i+1 < len(parts) {
					jobID, convErr := strconv.ParseInt(parts[i+1], 10, 64)
					if convErr != nil {
						break
					}
					if _, ok := targetKeys[jobID]; !ok {
						targetKeys[jobID] = make(map[string]struct{})
						if target != nil {
							*target = append(*target, parts[i+1])
						}
					}
					targetKeys[jobID][key] = struct{}{}
					break
				}
			}
		}
	}

	return result, nil
}

func (m *JobMarkers) HasCompletedMarker(jobID int64, key string) bool {
	return m.hasMarker(m.completedKeys, jobID, key)
}

func (m *JobMarkers) HasStartedMarker(jobID int64, key string) bool {
	return m.hasMarker(m.startedKeys, jobID, key)
}

// AnyCompletedKey returns an arbitrary completed-marker key for the given job,
// regardless of run_id. This is a fallback for when the current latest_run_id
// doesn't match the run that actually wrote the marker (e.g., after
// cleanupStaleAttempts replaced the attempt).
func (m *JobMarkers) AnyCompletedKey(jobID int64) (string, bool) {
	if m == nil {
		return "", false
	}
	keys, ok := m.completedKeys[jobID]
	if !ok || len(keys) == 0 {
		return "", false
	}
	for k := range keys {
		return k, true
	}
	return "", false
}

// IsProcessed returns true if any .processed marker exists for the given job.
func (m *JobMarkers) IsProcessed(jobID int64) bool {
	if m == nil {
		return false
	}
	_, ok := m.processedKeys[jobID]
	return ok
}

func (m *JobMarkers) hasMarker(markers map[int64]map[string]struct{}, jobID int64, key string) bool {
	if m == nil || key == "" {
		return false
	}
	keys, ok := markers[jobID]
	if !ok {
		return false
	}
	_, ok = keys[key]
	return ok
}

// ListCompleted returns job IDs that have a .complete marker but no .processed
// marker under the given prefix. Jobs that have already been processed are
// excluded so the caller doesn't re-ingest them.
func (c *Client) ListCompleted(ctx context.Context, prefix string) ([]string, error) {
	markers, err := c.ListJobMarkers(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var unprocessed []string
	for _, idStr := range markers.Completed {
		jobID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		if !markers.IsProcessed(jobID) {
			unprocessed = append(unprocessed, idStr)
		}
	}
	return unprocessed, nil
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

// GetObject retrieves the contents of a single object by key.
func (c *Client) GetObject(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object %s: %w", key, err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// PutMarker writes an empty object as a marker key.
func (c *Client) PutMarker(ctx context.Context, key string) error {
	return c.PutObject(ctx, key, strings.NewReader(""), "application/octet-stream")
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

// ObjectInfo holds basic metadata about an R2 object.
type ObjectInfo struct {
	Key       string
	SizeBytes int64
}

// ListObjects returns all objects under a prefix.
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	}

	var result []ObjectInfo
	paginator := s3.NewListObjectsV2Paginator(c.s3, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, obj := range page.Contents {
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			result = append(result, ObjectInfo{
				Key:       aws.ToString(obj.Key),
				SizeBytes: size,
			})
		}
	}
	return result, nil
}

// PutObject uploads data to R2 under the given key.
func (c *Client) PutObject(ctx context.Context, key string, body io.Reader, contentType string) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}
	_, err := c.s3.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("put object %s: %w", key, err)
	}
	return nil
}

// ObjectExists checks whether an object exists at the given key.
func (c *Client) ObjectExists(ctx context.Context, key string) (bool, error) {
	_, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// HeadObject returns a NotFound-style error when the key doesn't exist.
		// The S3 SDK wraps this as a smithy OperationError; check the HTTP status.
		if isS3NotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("head object %s: %w", key, err)
	}
	return true, nil
}

// Bucket returns the bucket name this client is configured for.
func (c *Client) Bucket() string {
	return c.bucket
}

// Endpoint returns the R2 endpoint URL.
func (c *Client) Endpoint() string {
	return c.endpoint
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

// IsNotFound returns true if the error indicates the object was not found
// (S3 NotFound or R2 NoSuchKey).
func IsNotFound(err error) bool {
	return isS3NotFound(err)
}

// isS3NotFound returns true if the error indicates the object was not found.
func isS3NotFound(err error) bool {
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	// R2 may return NoSuchKey instead of NotFound
	var noSuchKey *types.NoSuchKey
	return errors.As(err, &noSuchKey)
}
