// Package r2 provides a client for Cloudflare R2 (S3-compatible) storage.
// It is used to retrieve job results uploaded by Vast.ai instances.
package r2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithy "github.com/aws/smithy-go"
)

// Client wraps an S3-compatible client configured for Cloudflare R2.
type Client struct {
	s3       *s3.Client
	bucket   string
	endpoint string
	// multipartThresholdBytes and multipartPartSizeBytes override the
	// multipart routing threshold and part size. Zero selects the defaults.
	// Tests lower them to exercise the multipart path with small payloads.
	multipartThresholdBytes int64
	multipartPartSizeBytes  int64
}

const (
	// defaultMultipartThresholdBytes is the object size at or above which
	// PutObject switches from a single request to an S3 multipart upload.
	// Matches the S3 upload manager's default threshold; the 24.5 MiB agent
	// binary is the motivating large object, since a single-shot upload must
	// finish inside the same staging budget as the source tarballs and a
	// stalled transfer forfeits the whole object.
	defaultMultipartThresholdBytes = 16 << 20
	// defaultMultipartPartSizeBytes matches the S3 upload manager's default
	// part size and satisfies the 5 MiB minimum for non-final parts.
	defaultMultipartPartSizeBytes = 8 << 20
	// multipartPartAttempts bounds the explicit per-part retries layered on
	// top of the SDK's request-level retryer, so a failed part is resent
	// without restarting the upload.
	multipartPartAttempts = 3
)

type progressWriter struct {
	w          io.Writer
	onProgress func()
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 && w.onProgress != nil {
		w.onProgress()
	}
	return n, err
}

func copyWithIdleTimeout(ctx context.Context, cancel context.CancelFunc, src io.Reader, dst io.Writer, idleTimeout time.Duration) (int64, error) {
	if idleTimeout <= 0 {
		return io.Copy(dst, src)
	}

	progressCh := make(chan struct{}, 1)
	timeoutCh := make(chan error, 1)
	monitorDone := make(chan struct{})

	go func() {
		defer close(monitorDone)
		timer := time.NewTimer(idleTimeout)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-progressCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idleTimeout)
			case <-timer.C:
				timeoutCh <- fmt.Errorf("download stalled: no progress for %s", idleTimeout)
				cancel()
				return
			}
		}
	}()

	progDst := &progressWriter{
		w: dst,
		onProgress: func() {
			select {
			case progressCh <- struct{}{}:
			default:
			}
		},
	}

	n, copyErr := io.Copy(progDst, src)
	cancel()
	<-monitorDone

	select {
	case stallErr := <-timeoutCh:
		return n, stallErr
	default:
	}
	if copyErr != nil {
		return n, copyErr
	}
	return n, nil
}

// IsConfigured returns true if the client has a usable S3 connection.
func (c *Client) IsConfigured() bool {
	return c != nil && c.s3 != nil
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

// HasUnprocessedComplete reports whether jobID has at least one .complete
// marker whose paired .processed marker is absent. The pairing rule is the
// natural one: replace the .complete suffix with .processed. This works
// uniformly for per-attempt keys (`jobs/X/runs/N/.complete` ↔
// `jobs/X/runs/N/.processed`) and the inventory-fallback job-scoped form
// (`jobs/X/.complete` ↔ `jobs/X/.processed`).
//
// This is the per-attempt gate used by the cloud sync loop: an older attempt
// being processed must not suppress reconciliation of a newer attempt's
// .complete marker on the same job.
func (m *JobMarkers) HasUnprocessedComplete(jobID int64) bool {
	if m == nil {
		return false
	}
	completes, ok := m.completedKeys[jobID]
	if !ok {
		return false
	}
	processed := m.processedKeys[jobID]
	for completeKey := range completes {
		processedKey := PairedProcessedKey(completeKey)
		if _, done := processed[processedKey]; !done {
			return true
		}
	}
	return false
}

// CompletedKeysForJob returns the raw set of .complete-marker keys observed
// for jobID. Returns nil when no markers exist. The map is owned by the
// JobMarkers; callers must not mutate it.
func (m *JobMarkers) CompletedKeysForJob(jobID int64) map[string]struct{} {
	if m == nil {
		return nil
	}
	return m.completedKeys[jobID]
}

// PairedProcessedKey derives the .processed key for a given .complete key by
// swapping the suffix. See HasUnprocessedComplete.
func PairedProcessedKey(completeKey string) string {
	return strings.TrimSuffix(completeKey, "/.complete") + "/.processed"
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
			relPath, ok := relPathForKey(key, prefix)
			if !ok {
				continue
			}

			localPath := filepath.Join(localDir, filepath.FromSlash(relPath))
			if err := c.downloadObject(ctx, key, localPath); err != nil {
				return fmt.Errorf("download %s: %w", key, err)
			}
		}
	}

	return nil
}

// relPathForKey derives the local relative path for an object key under
// prefix. Listing-derived paths must not escape the destination directory.
func relPathForKey(key, prefix string) (string, bool) {
	rel := strings.TrimPrefix(key, prefix)
	rel = strings.TrimPrefix(rel, "/")
	cleaned := path.Clean(rel)
	if cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

// GetObjectReader retrieves a single object body by key. The caller must close it.
func (c *Client) GetObjectReader(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object %s: %w", key, err)
	}
	return resp.Body, nil
}

// GetObject retrieves the contents of a single object by key.
func (c *Client) GetObject(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.GetObjectReader(ctx, key)
	if err != nil {
		return nil, err
	}
	defer resp.Close()
	return io.ReadAll(resp)
}

// GetObjectRange retrieves bytes from `fromOffset` to the end of the
// object via an HTTP Range request. Used by the opslog sync to pull only
// the new tail of an append-only JSONL — the alternative (refetching the
// whole file every pass for an active rental) consistently exceeded the
// 10s per-object timeout for long-running instances and failed every
// pass with no data exchanged.
//
// fromOffset must be non-negative. If fromOffset == size, R2 returns
// 416 InvalidRange (treated as ErrRangeNotSatisfiable so callers can
// short-circuit). If fromOffset > size (cache survived a server-side
// rewrite), the same error fires; callers should fall back to
// GetObject for a full refresh.
func (c *Client) GetObjectRange(ctx context.Context, key string, fromOffset int64) ([]byte, error) {
	if fromOffset < 0 {
		return nil, fmt.Errorf("get object range %s: negative offset %d", key, fromOffset)
	}
	rangeHeader := fmt.Sprintf("bytes=%d-", fromOffset)
	resp, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		if isS3InvalidRange(err) {
			return nil, ErrRangeNotSatisfiable
		}
		return nil, fmt.Errorf("get object range %s: %w", key, err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ErrRangeNotSatisfiable signals that the requested byte range starts at
// or past the end of the object. Used by GetObjectRange so callers can
// distinguish "no new bytes" from network errors.
var ErrRangeNotSatisfiable = errors.New("requested byte range not satisfiable")

func isS3InvalidRange(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "InvalidRange", "RequestedRangeNotSatisfiable":
			return true
		}
	}
	return false
}

// GetObjectWithMeta retrieves both the body and the object's LastModified
// timestamp. Used by completion sync to recover an authoritative end_time when
// the agent's completion JSON is missing — the marker's R2 LastModified is the
// closest proxy to the true completion time (uploaded by the agent immediately
// after the job exits).
func (c *Client) GetObjectWithMeta(ctx context.Context, key string) ([]byte, time.Time, error) {
	resp, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("get object %s: %w", key, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, time.Time{}, err
	}
	var lm time.Time
	if resp.LastModified != nil {
		lm = *resp.LastModified
	}
	return body, lm, nil
}

// PutMarker writes an empty object as a marker key.
func (c *Client) PutMarker(ctx context.Context, key string) error {
	return c.PutObject(ctx, key, strings.NewReader(""), "application/octet-stream")
}

// DeleteObject deletes a single object by key.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
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
			if err := c.DeleteObject(ctx, aws.ToString(obj.Key)); err != nil {
				return err
			}
		}
	}

	return nil
}

// ObjectInfo holds basic metadata about an R2 object.
type ObjectInfo struct {
	Key          string
	SizeBytes    int64
	LastModified time.Time
	ETag         string
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
			var lastModified time.Time
			if obj.LastModified != nil {
				lastModified = *obj.LastModified
			}
			result = append(result, ObjectInfo{
				Key:          aws.ToString(obj.Key),
				SizeBytes:    size,
				LastModified: lastModified,
				ETag:         aws.ToString(obj.ETag),
			})
		}
	}
	return result, nil
}

// ListObjectsLimited returns at most maxObjects objects under prefix. Complete
// is true only when R2 confirmed that the returned page sequence exhausted the
// prefix. Callers may use a complete listing to infer absence; a truncated
// listing can confirm only the objects it returned.
func (c *Client) ListObjectsLimited(ctx context.Context, prefix string, maxObjects int) ([]ObjectInfo, bool, error) {
	if maxObjects <= 0 {
		return nil, false, fmt.Errorf("list objects under %s: max objects must be positive", prefix)
	}
	input := &s3.ListObjectsV2Input{
		Bucket:  aws.String(c.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: aws.Int32(int32(min(maxObjects, 1000))),
	}

	result := make([]ObjectInfo, 0, min(maxObjects, 1000))
	paginator := s3.NewListObjectsV2Paginator(c.s3, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("list objects under %s: %w", prefix, err)
		}
		remaining := maxObjects - len(result)
		contents := page.Contents
		if len(contents) > remaining {
			contents = contents[:remaining]
		}
		for _, obj := range contents {
			var size int64
			if obj.Size != nil {
				size = *obj.Size
			}
			var lastModified time.Time
			if obj.LastModified != nil {
				lastModified = *obj.LastModified
			}
			result = append(result, ObjectInfo{
				Key:          aws.ToString(obj.Key),
				SizeBytes:    size,
				LastModified: lastModified,
				ETag:         aws.ToString(obj.ETag),
			})
		}
		if len(result) == maxObjects {
			return result, len(page.Contents) <= remaining && !paginator.HasMorePages(), nil
		}
	}
	return result, true, nil
}

// PutObject uploads data to R2 under the given key. Objects at or above the
// multipart threshold upload via S3 multipart with per-part retry, so a
// stalled or failed part is resent without forfeiting the whole transfer;
// smaller objects use a single PutObject.
//
// A seekable body is streamed part by part without full client-side
// buffering. Whether its reads pace with the network is up to the SDK's
// internal buffering; callers that derive progress from body reads must use
// PutObjectWithPartProgress instead.
func (c *Client) PutObject(ctx context.Context, key string, body io.Reader, contentType string) error {
	return c.PutObjectWithPartProgress(ctx, key, body, contentType, nil)
}

// PutObjectWithPartProgress is PutObject with onPart invoked after each
// multipart part is uploaded (with the part's byte count). The SDK may serve
// a part from an internal buffer rather than reading the caller's body
// during the transfer, so callers whose progress signals depend on body
// reads — the sync upload stall watchdog — must feed them from this callback
// instead. The callback fires client-side and is synchronous; the single
// PutObject path ignores it because its body streams through the SDK
// directly and read-derived progress already works there.
func (c *Client) PutObjectWithPartProgress(ctx context.Context, key string, body io.Reader, contentType string, onPart func(int64)) error {
	threshold := c.multipartThreshold()
	if size, ok := readerSize(body); ok {
		if size >= threshold {
			return c.putObjectMultipart(ctx, key, body, contentType, size, onPart)
		}
		return c.putObjectSingle(ctx, key, body, contentType)
	}
	// Unknown size: buffer up to the threshold. Bodies that fit stay
	// single-shot; larger ones stream the buffered prefix plus the
	// remainder through the buffered multipart path.
	prefix, rest, err := bufferUpTo(body, threshold)
	if err != nil {
		return fmt.Errorf("read object %s: %w", key, err)
	}
	if rest == nil {
		return c.putObjectSingle(ctx, key, bytes.NewReader(prefix), contentType)
	}
	return c.putObjectMultipart(ctx, key, io.MultiReader(bytes.NewReader(prefix), rest), contentType, 0, onPart)
}

func (c *Client) multipartThreshold() int64 {
	if c.multipartThresholdBytes > 0 {
		return c.multipartThresholdBytes
	}
	return defaultMultipartThresholdBytes
}

func (c *Client) multipartPartSize() int64 {
	if c.multipartPartSizeBytes > 0 {
		return c.multipartPartSizeBytes
	}
	return defaultMultipartPartSizeBytes
}

// readerSize reports the remaining readable length of body when it can be
// determined without consuming data.
func readerSize(body io.Reader) (int64, bool) {
	switch b := body.(type) {
	case *bytes.Reader:
		return int64(b.Len()), true
	case *strings.Reader:
		return int64(b.Len()), true
	case io.Seeker:
		cur, err := b.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, false
		}
		end, err := b.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, false
		}
		if _, err := b.Seek(cur, io.SeekStart); err != nil {
			return 0, false
		}
		return end - cur, true
	}
	return 0, false
}

// bufferUpTo reads up to limit bytes from r. rest is nil when r was fully
// consumed within the limit; otherwise rest yields the buffered overflow
// followed by the unread remainder of r.
func bufferUpTo(r io.Reader, limit int64) (prefix []byte, rest io.Reader, err error) {
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(buf)) <= limit {
		return buf, nil, nil
	}
	return buf[:limit], io.MultiReader(bytes.NewReader(buf[limit:]), r), nil
}

func (c *Client) putObjectSingle(ctx context.Context, key string, body io.Reader, contentType string) error {
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

// putObjectMultipart uploads body as an S3 multipart upload. Parts are sent
// sequentially; each part is retried up to multipartPartAttempts times, and
// any failure aborts the upload so orphaned parts are not left in the bucket.
func (c *Client) putObjectMultipart(ctx context.Context, key string, body io.Reader, contentType string, size int64, onPart func(int64)) error {
	createInput := &s3.CreateMultipartUploadInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}
	if contentType != "" {
		createInput.ContentType = aws.String(contentType)
	}
	create, err := c.s3.CreateMultipartUpload(ctx, createInput)
	if err != nil {
		return fmt.Errorf("create multipart upload %s: %w", key, err)
	}
	uploadID := create.UploadId

	// abort uses a detached context because ctx is usually already canceled
	// or exhausted by the time a failure surfaces.
	abort := func(cause error) error {
		abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = c.s3.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(c.bucket),
			Key:      aws.String(key),
			UploadId: uploadID,
		})
		return cause
	}

	// A seekable body with a known size streams part by part: each part
	// reads through the caller's reader at network pace, so read-derived
	// progress signals stay live for the whole transfer. Buffering instead
	// would front-load the reads and leave the wire idle for the rest of
	// each part, which the sync upload stall watchdog reads as a stall
	// (wb158 review). Non-seekable bodies have no choice but to buffer.
	seeker, seekable := body.(interface {
		io.Reader
		io.Seeker
	})
	if !seekable || size <= 0 {
		return c.putObjectMultipartBuffered(ctx, key, body, contentType, uploadID, abort, onPart)
	}

	partSize := c.multipartPartSize()
	var parts []types.CompletedPart
	partNumber := int32(1)
	for offset := int64(0); offset < size; offset, partNumber = offset+partSize, partNumber+1 {
		length := partSize
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		makePart := func() (io.Reader, int64, error) {
			return &sectionReadSeeker{r: seeker, s: seeker, base: offset, limit: length, underPos: -1}, length, nil
		}
		etag, err := c.uploadPartWithRetry(ctx, key, uploadID, partNumber, makePart)
		if err != nil {
			return abort(fmt.Errorf("upload part %d of %s: %w", partNumber, key, err))
		}
		if onPart != nil {
			onPart(length)
		}
		parts = append(parts, types.CompletedPart{
			ETag:       aws.String(etag),
			PartNumber: aws.Int32(partNumber),
		})
	}

	_, err = c.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(c.bucket),
		Key:             aws.String(key),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return abort(fmt.Errorf("complete multipart upload %s: %w", key, err))
	}
	return nil
}

// putObjectMultipartBuffered is the multipart path for bodies that cannot
// seek: each part is read into memory up front and resent from the buffer on
// retry. Reads of the caller's reader concentrate before each part transfer,
// so callers whose progress depends on read pacing should pass a seekable
// body for large objects.
func (c *Client) putObjectMultipartBuffered(ctx context.Context, key string, body io.Reader, contentType string, uploadID *string, abort func(error) error, onPart func(int64)) error {
	buf := make([]byte, c.multipartPartSize())
	var parts []types.CompletedPart
	partNumber := int32(1)
	for {
		n, readErr := io.ReadFull(body, buf)
		if readErr == io.EOF && n == 0 {
			break
		}
		if readErr != nil && readErr != io.ErrUnexpectedEOF {
			return abort(fmt.Errorf("read object %s: %w", key, readErr))
		}
		part := make([]byte, n)
		copy(part, buf[:n])
		etag, err := c.uploadPartWithRetry(ctx, key, uploadID, partNumber, func() (io.Reader, int64, error) {
			return bytes.NewReader(part), int64(n), nil
		})
		if err != nil {
			return abort(fmt.Errorf("upload part %d of %s: %w", partNumber, key, err))
		}
		if onPart != nil {
			onPart(int64(n))
		}
		parts = append(parts, types.CompletedPart{
			ETag:       aws.String(etag),
			PartNumber: aws.Int32(partNumber),
		})
		partNumber++
		if readErr == io.ErrUnexpectedEOF {
			break
		}
	}

	_, err := c.s3.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(c.bucket),
		Key:             aws.String(key),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return abort(fmt.Errorf("complete multipart upload %s: %w", key, err))
	}
	return nil
}

// sectionReadSeeker presents a fixed-length window of an underlying
// reader-and-seeker as a seekable stream. The SDK's payload hashing seeks
// the part body back to its start on (re)send, while the reads themselves
// keep flowing through the underlying reader at network pace — callers that
// derive progress from those reads (the sync upload stall watchdog) stay
// live across retries.
//
// underPos tracks the underlying stream's position and starts at -1
// (unknown): several windows are created over one shared underlying seeker
// by consecutive part uploads, so assuming position 0 would re-send bytes
// from a previous part's window (wb158 review).
type sectionReadSeeker struct {
	r        io.Reader
	s        io.Seeker
	base     int64 // window start within the underlying stream
	limit    int64 // window length
	underPos int64 // current position in the underlying stream; -1 = unknown
	pos      int64 // current position within the window
}

func (s *sectionReadSeeker) Read(p []byte) (int, error) {
	if s.pos >= s.limit {
		return 0, io.EOF
	}
	if int64(len(p)) > s.limit-s.pos {
		p = p[:s.limit-s.pos]
	}
	if s.underPos != s.base+s.pos {
		if _, err := s.s.Seek(s.base+s.pos, io.SeekStart); err != nil {
			return 0, err
		}
		s.underPos = s.base + s.pos
	}
	n, err := s.r.Read(p)
	s.pos += int64(n)
	s.underPos += int64(n)
	return n, err
}

func (s *sectionReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += s.pos
	case io.SeekEnd:
		offset += s.limit
	}
	if offset < 0 {
		return 0, fmt.Errorf("seek before window start")
	}
	s.pos = offset
	return s.pos, nil
}

// uploadPartWithRetry uploads one part, retrying failed attempts with a
// linear backoff. makePart supplies a fresh reader over the part payload for
// every attempt, so a streaming part re-reads from its source and a buffered
// part resends the same bytes.
func (c *Client) uploadPartWithRetry(ctx context.Context, key string, uploadID *string, partNumber int32, makePart func() (io.Reader, int64, error)) (string, error) {
	var lastErr error
	for attempt := range multipartPartAttempts {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}
		part, length, err := makePart()
		if err != nil {
			return "", err
		}
		out, err := c.s3.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(c.bucket),
			Key:           aws.String(key),
			UploadId:      uploadID,
			PartNumber:    aws.Int32(partNumber),
			Body:          part,
			ContentLength: aws.Int64(length),
		})
		if err == nil {
			return aws.ToString(out.ETag), nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		lastErr = err
	}
	return "", lastErr
}

// ErrPreconditionFailed signals that an R2 conditional write or read failed
// its ETag / existence precondition. This is the expected contention result
// for blackboard-style claims.
var ErrPreconditionFailed = errors.New("r2 precondition failed")

// PutObjectConditional uploads an object using S3 conditional headers. Pass
// ifNoneMatch="*" to create only when the key does not exist, or ifMatch=<etag>
// to update only when the current object still has that ETag.
func (c *Client) PutObjectConditional(ctx context.Context, key string, body io.Reader, contentType, ifMatch, ifNoneMatch string) (string, error) {
	input := &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}
	if ifMatch != "" {
		input.IfMatch = aws.String(ifMatch)
	}
	if ifNoneMatch != "" {
		input.IfNoneMatch = aws.String(ifNoneMatch)
	}
	out, err := c.s3.PutObject(ctx, input)
	if err != nil {
		if isS3PreconditionFailed(err) {
			return "", ErrPreconditionFailed
		}
		return "", fmt.Errorf("put object %s: %w", key, err)
	}
	return aws.ToString(out.ETag), nil
}

// HeadObject returns metadata for a single object, including its ETag.
func (c *Client) HeadObject(ctx context.Context, key string) (*ObjectInfo, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("head object %s: %w", key, err)
		}
		return nil, fmt.Errorf("head object %s: %w", key, err)
	}
	size := int64(0)
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	var lastModified time.Time
	if out.LastModified != nil {
		lastModified = *out.LastModified
	}
	return &ObjectInfo{
		Key:          key,
		SizeBytes:    size,
		LastModified: lastModified,
		ETag:         aws.ToString(out.ETag),
	}, nil
}

// PresignPutURL returns a presigned URL that can PUT to the given key for
// up to ttl. Used by the OnStart probe so the container can write a marker
// without rclone or aws-cli — just `curl -X PUT --upload-file -`.
func (c *Client) PresignPutURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if c == nil || c.s3 == nil {
		return "", fmt.Errorf("r2 client not configured")
	}
	presignClient := s3.NewPresignClient(c.s3)
	req, err := presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign put %s: %w", key, err)
	}
	return req.URL, nil
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

// ObjectSize returns the size in bytes of the object at the given key.
// Returns -1 if the object does not exist.
func (c *Client) ObjectSize(ctx context.Context, key string) (int64, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return -1, nil
		}
		return 0, fmt.Errorf("head object %s: %w", key, err)
	}
	if out.ContentLength == nil {
		return 0, nil
	}
	return *out.ContentLength, nil
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

// DownloadObjectToWriterWithIdleTimeout streams an object to a writer and only
// fails when no transfer progress is made for idleTimeout.
func (c *Client) DownloadObjectToWriterWithIdleTimeout(parent context.Context, key string, dst io.Writer, idleTimeout time.Duration) (int64, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	body, err := c.GetObjectReader(ctx, key)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	return copyWithIdleTimeout(ctx, cancel, body, dst, idleTimeout)
}

// DownloadObjectToFileWithIdleTimeout streams an object to localPath and only
// fails when no transfer progress is made for idleTimeout.
func (c *Client) DownloadObjectToFileWithIdleTimeout(parent context.Context, key string, localPath string, idleTimeout time.Duration) (int64, error) {
	dir := filepath.Dir(localPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	f, err := createDownloadTempFile(dir, filepath.Base(localPath))
	if err != nil {
		return 0, fmt.Errorf("write %s: %w", localPath, err)
	}
	tmpPath := f.Name()
	n, copyErr := c.DownloadObjectToWriterWithIdleTimeout(parent, key, f, idleTimeout)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		if closeErr != nil {
			return n, errors.Join(copyErr, fmt.Errorf("write %s: %w", localPath, closeErr))
		}
		return n, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return n, fmt.Errorf("write %s: %w", localPath, closeErr)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
		return n, fmt.Errorf("write %s: %w", localPath, err)
	}
	return n, nil
}

func createDownloadTempFile(dir, base string) (*os.File, error) {
	var lastErr error
	for i := range 100 {
		name := "." + base + ".tmp." + strconv.Itoa(os.Getpid()) + "." + strconv.FormatInt(time.Now().UnixNano(), 36) + "." + strconv.Itoa(i)
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o666)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// DownloadResultsWithIdleTimeout downloads all files under prefix to localDir
// using per-object idle timeout semantics.
func (c *Client) DownloadResultsWithIdleTimeout(ctx context.Context, prefix string, localDir string, idleTimeout time.Duration) error {
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
			relPath, ok := relPathForKey(key, prefix)
			if !ok {
				continue
			}
			localPath := filepath.Join(localDir, filepath.FromSlash(relPath))
			if _, err := c.DownloadObjectToFileWithIdleTimeout(ctx, key, localPath, idleTimeout); err != nil {
				return fmt.Errorf("download %s: %w", key, err)
			}
		}
	}
	return nil
}

// IsNotFound returns true if the error indicates the object was not found
// (S3 NotFound or R2 NoSuchKey).
func IsNotFound(err error) bool {
	return isS3NotFound(err)
}

// IsPreconditionFailed returns true when err is ErrPreconditionFailed or an
// SDK error equivalent to HTTP 412.
func IsPreconditionFailed(err error) bool {
	return errors.Is(err, ErrPreconditionFailed) || isS3PreconditionFailed(err)
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

func isS3PreconditionFailed(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "PreconditionFailed", "ConditionalRequestConflict":
			return true
		}
	}
	var responseErr interface{ HTTPStatusCode() int }
	return errors.As(err, &responseErr) && responseErr.HTTPStatusCode() == http.StatusPreconditionFailed
}
