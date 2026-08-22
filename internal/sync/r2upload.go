package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/r2"
)

const (
	sourceExistenceConcurrency    = 32
	sourceListConcurrency         = 8
	sourceListMinObjectsPerPrefix = 128
	sourceListScanMultiplier      = 3
	sourceListMinObjectsPerScan   = 1000
	sourceListMaxObjectsPerScan   = 10000
	sourceSlowDiscoveryThreshold  = 5 * time.Second
	sourceClosureReceiptVersion   = "v1"
)

// UploadSourceProgressFunc receives coarse source upload phase updates.
// Phases are descriptive labels such as "hashing source", "checking cache",
// "uploading source", and "ready".
type UploadSourceProgressFunc func(phase string)

// SourceUploadResult describes the uploaded multi-root source state.
type SourceUploadResult struct {
	Manifest SourceManifest
	Stats    SourceUploadStats
}

// SourceUploadStats describes where source staging spent its time and how R2
// cache discovery was resolved. It is emitted to structured logs for large or
// slow snapshots and retained in the result for focused diagnostics and tests.
type SourceUploadStats struct {
	ReceiptHit            bool
	ReceiptLookupFailed   bool
	ReceiptWriteFailed    bool
	Objects               int
	ListPrefixes          int
	ListCompletePrefixes  int
	ListTruncatedPrefixes int
	ListFailedPrefixes    int
	ListedObjects         int
	HeadRequests          int
	CacheHits             int
	CacheMisses           int
	UploadedObjects       int
	HashDuration          time.Duration
	DiscoveryDuration     time.Duration
	UploadDuration        time.Duration
}

type sourceObjectStore interface {
	ObjectExists(context.Context, string) (bool, error)
	ListObjectsLimited(context.Context, string, int) ([]r2.ObjectInfo, bool, error)
	PutObject(context.Context, string, io.Reader, string) error
	PutObjectConditional(context.Context, string, io.Reader, string, string, string) (string, error)
}

type sourceUploadObject struct {
	key         string
	digest      string
	label       string
	contentType string
	progress    string
	open        func() (*os.File, func(), error)
}

type sourceClosureReceipt struct {
	Version      string                       `json:"version"`
	ManifestHash string                       `json:"manifest_hash"`
	Objects      []sourceClosureReceiptObject `json:"objects"`
}

type sourceClosureReceiptObject struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
}

type sourcePresence uint8

const (
	sourcePresenceUnknown sourcePresence = iota
	sourcePresencePresent
	sourcePresenceAbsent
)

// UploadSourceToR2 creates a content-addressed tarball of localDir and uploads
// it to R2 if it doesn't already exist. Returns the R2 key.
func UploadSourceToR2(ctx context.Context, r2Client *r2.Client, localDir string) (string, error) {
	return UploadSourceToR2ForInputs(ctx, r2Client, localDir, nil)
}

// UploadSourceToR2ForInputs creates a content-addressed tarball for localDir,
// ensuring declared local: inputs are included even if the source snapshot would
// normally exclude them.
func UploadSourceToR2ForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string) (string, error) {
	return UploadSourceToR2WithProgressForInputs(ctx, r2Client, localDir, inputs, nil)
}

// UploadSourceToR2WithProgress creates a content-addressed tarball of localDir,
// uploads it to R2 if needed, and reports coarse phase changes via onProgress.
func UploadSourceToR2WithProgress(ctx context.Context, r2Client *r2.Client, localDir string, onProgress UploadSourceProgressFunc) (string, error) {
	return UploadSourceToR2WithProgressForInputs(ctx, r2Client, localDir, nil, onProgress)
}

// UploadSourceToR2WithProgressForInputs creates a content-addressed tarball of
// localDir, overlays declared local: inputs when needed, uploads it to R2 if
// needed, and reports coarse phase changes via onProgress.
func UploadSourceToR2WithProgressForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string, onProgress UploadSourceProgressFunc) (string, error) {
	result, err := uploadSourceRootsToR2(ctx, r2Client, localDir, inputs, nil, onProgress, false)
	if err != nil {
		return "", err
	}
	if len(result.Manifest.Roots) == 0 {
		return "", fmt.Errorf("source manifest for %s has no roots", localDir)
	}
	return result.Manifest.Roots[0].R2Key, nil
}

// UploadCloudSourceRootsToR2ForInputs uploads cloud source roots, diverting
// large files into content-addressed blob objects.
func UploadCloudSourceRootsToR2ForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string) (SourceUploadResult, error) {
	return UploadCloudSourceRootsToR2WithProgressForInputsAndCommands(ctx, r2Client, localDir, inputs, nil, nil)
}

// UploadCloudSourceRootsToR2WithProgressForInputsAndCommands uploads the
// cloud snapshot for a submitted command, including command-derived sibling
// roots and diverted large-file blobs.
func UploadCloudSourceRootsToR2WithProgressForInputsAndCommands(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string, commands []string, onProgress UploadSourceProgressFunc) (SourceUploadResult, error) {
	return uploadSourceRootsToR2(ctx, r2Client, localDir, inputs, commands, onProgress, true)
}

// UploadSourceRootsToR2WithProgressForInputs uploads each content-addressed
// source root tarball and returns the ordered manifest for the whole source
// state.
func UploadSourceRootsToR2WithProgressForInputs(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string, onProgress UploadSourceProgressFunc) (SourceUploadResult, error) {
	return UploadSourceRootsToR2WithProgressForInputsAndCommands(ctx, r2Client, localDir, inputs, nil, onProgress)
}

// UploadSourceRootsToR2WithProgressForInputsAndCommands uploads source roots
// including project/script-derived uv path sources from commands.
func UploadSourceRootsToR2WithProgressForInputsAndCommands(ctx context.Context, r2Client *r2.Client, localDir string, inputs []string, commands []string, onProgress UploadSourceProgressFunc) (SourceUploadResult, error) {
	return uploadSourceRootsToR2(ctx, r2Client, localDir, inputs, commands, onProgress, true)
}

func uploadSourceRootsToR2(ctx context.Context, store sourceObjectStore, localDir string, inputs []string, commands []string, onProgress UploadSourceProgressFunc, divertLargeFiles bool) (SourceUploadResult, error) {
	if onProgress == nil {
		onProgress = func(string) {}
	}

	stats := SourceUploadStats{}
	hashStarted := time.Now()
	onProgress("hashing source")
	build, err := buildSourceManifestForInputsAndCommands(localDir, inputs, commands, divertLargeFiles)
	if err != nil {
		return SourceUploadResult{}, fmt.Errorf("create source manifest: %w", err)
	}
	stats.HashDuration = time.Since(hashStarted)
	defer build.cleanup()
	defer removeFiles(build.tarPaths)
	manifest := build.manifest
	objects, err := buildSourceUploadObjects(build)
	if err != nil {
		return SourceUploadResult{}, err
	}
	stats.Objects = len(objects)

	discoveryStarted := time.Now()
	receiptKey := dataplane.SourceClosureReceipt(manifest.Hash)
	onProgress("checking source receipt")
	receiptExists, receiptErr := store.ObjectExists(ctx, receiptKey)
	if receiptErr != nil {
		stats.ReceiptLookupFailed = true
		slog.Warn("source closure receipt lookup failed; falling back to object discovery",
			"component", "sync", "receipt_key", receiptKey, "error", receiptErr)
	} else if receiptExists {
		stats.ReceiptHit = true
		stats.CacheHits = len(objects)
		stats.DiscoveryDuration = time.Since(discoveryStarted)
		onProgress("ready")
		logSourceUploadStats(ctx, stats)
		return SourceUploadResult{Manifest: manifest, Stats: stats}, nil
	}

	presence := make(map[string]sourcePresence, len(objects))
	onProgress("listing source cache")
	discoverSourceObjectsByPrefix(ctx, store, objects, presence, &stats)

	unknown := make([]sourceUploadObject, 0, len(objects))
	for _, object := range objects {
		if presence[object.key] == sourcePresenceUnknown {
			unknown = append(unknown, object)
		}
	}
	if len(unknown) > 0 {
		onProgress("checking source cache")
		headPresence, requests, err := checkSourceObjectsConcurrently(ctx, store, unknown, sourceExistenceConcurrency)
		stats.HeadRequests += requests
		if err != nil {
			return SourceUploadResult{}, err
		}
		for key, exists := range headPresence {
			if exists {
				presence[key] = sourcePresencePresent
			} else {
				presence[key] = sourcePresenceAbsent
			}
		}
	}
	stats.DiscoveryDuration = time.Since(discoveryStarted)

	missing := make([]sourceUploadObject, 0, len(objects))
	for _, object := range objects {
		switch presence[object.key] {
		case sourcePresencePresent:
			stats.CacheHits++
		case sourcePresenceAbsent:
			stats.CacheMisses++
			missing = append(missing, object)
		default:
			return SourceUploadResult{}, fmt.Errorf("source cache state for %s remained unknown", object.label)
		}
	}

	uploadStarted := time.Now()
	for _, object := range missing {
		onProgress(object.progress)
		if err := uploadSourceObject(ctx, store, object); err != nil {
			return SourceUploadResult{}, err
		}
		stats.UploadedObjects++
	}
	stats.UploadDuration = time.Since(uploadStarted)

	receiptBody, err := marshalSourceClosureReceipt(manifest.Hash, objects)
	if err != nil {
		return SourceUploadResult{}, err
	}
	if _, err := store.PutObjectConditional(ctx, receiptKey, bytes.NewReader(receiptBody), "application/json", "", "*"); err != nil && !r2.IsPreconditionFailed(err) {
		stats.ReceiptWriteFailed = true
		slog.Warn("source objects are ready but closure receipt publication failed",
			"component", "sync", "receipt_key", receiptKey, "error", err)
	}

	onProgress("ready")
	logSourceUploadStats(ctx, stats)
	return SourceUploadResult{Manifest: manifest, Stats: stats}, nil
}

func buildSourceUploadObjects(build sourceManifestBuild) ([]sourceUploadObject, error) {
	objects := make([]sourceUploadObject, 0, len(build.manifest.Roots))
	seen := make(map[string]string)
	add := func(object sourceUploadObject) error {
		if previousDigest, ok := seen[object.key]; ok {
			if previousDigest != object.digest {
				return fmt.Errorf("source object key %s has conflicting digests %s and %s", object.key, previousDigest, object.digest)
			}
			return nil
		}
		seen[object.key] = object.digest
		objects = append(objects, object)
		return nil
	}

	for i, root := range build.manifest.Roots {
		if i >= len(build.blobPaths) || len(build.blobPaths[i]) != len(root.Blobs) {
			return nil, fmt.Errorf("source root %s blob paths do not match its manifest", root.LocalPath)
		}
		for j, blob := range root.Blobs {
			blobPath := build.blobPaths[i][j]
			blobHash := blob.SHA256
			blobLabel := "source blob " + blob.RelPath
			if err := add(sourceUploadObject{
				key:         blob.R2Key,
				digest:      blobHash,
				label:       blobLabel,
				contentType: "application/octet-stream",
				progress:    "uploading source blob",
				open: func() (*os.File, func(), error) {
					return openSourceBlobSnapshot(blobPath, blobHash)
				},
			}); err != nil {
				return nil, err
			}
		}

		if i >= len(build.tarPaths) {
			return nil, fmt.Errorf("source root %s has no residual tarball", root.LocalPath)
		}
		tarPath := build.tarPaths[i]
		rootLabel := "source root " + root.LocalPath
		if err := add(sourceUploadObject{
			key:         root.R2Key,
			digest:      root.Hash,
			label:       rootLabel,
			contentType: "application/gzip",
			progress:    "uploading source",
			open: func() (*os.File, func(), error) {
				file, err := os.Open(tarPath)
				return file, func() {}, err
			},
		}); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

func uploadSourceObject(ctx context.Context, store sourceObjectStore, object sourceUploadObject) error {
	file, cleanup, err := object.open()
	if err != nil {
		return fmt.Errorf("open %s: %w", object.label, err)
	}
	defer cleanup()
	if err := store.PutObject(ctx, object.key, file, object.contentType); err != nil {
		_ = file.Close()
		return fmt.Errorf("upload %s: %w", object.label, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", object.label, err)
	}
	return nil
}

func marshalSourceClosureReceipt(manifestHash string, objects []sourceUploadObject) ([]byte, error) {
	receiptObjects := make([]sourceClosureReceiptObject, 0, len(objects))
	for _, object := range objects {
		if object.key == "" || object.digest == "" {
			return nil, fmt.Errorf("source closure %s contains an object without a key or digest", manifestHash)
		}
		receiptObjects = append(receiptObjects, sourceClosureReceiptObject{Key: object.key, SHA256: object.digest})
	}
	sort.Slice(receiptObjects, func(i, j int) bool { return receiptObjects[i].Key < receiptObjects[j].Key })
	receipt := sourceClosureReceipt{
		Version:      sourceClosureReceiptVersion,
		ManifestHash: manifestHash,
		Objects:      receiptObjects,
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("marshal source closure receipt: %w", err)
	}
	return data, nil
}

func discoverSourceObjectsByPrefix(ctx context.Context, store sourceObjectStore, objects []sourceUploadObject, presence map[string]sourcePresence, stats *SourceUploadStats) {
	groups := make(map[string][]sourceUploadObject)
	for _, object := range objects {
		prefix, ok := sourceObjectListPrefix(object.key)
		if ok {
			groups[prefix] = append(groups[prefix], object)
		}
	}

	prefixes := make([]string, 0, len(groups))
	for prefix, group := range groups {
		if len(group) >= sourceListMinObjectsPerPrefix {
			prefixes = append(prefixes, prefix)
		}
	}
	if len(prefixes) == 0 {
		return
	}
	sort.Strings(prefixes)

	type listResult struct {
		prefix   string
		objects  []r2.ObjectInfo
		complete bool
		err      error
	}
	jobs := make(chan string)
	results := make(chan listResult, len(prefixes))
	workerCount := min(sourceListConcurrency, len(prefixes))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for prefix := range jobs {
				limit := len(groups[prefix]) * sourceListScanMultiplier
				limit = max(limit, sourceListMinObjectsPerScan)
				limit = min(limit, sourceListMaxObjectsPerScan)
				listed, complete, err := store.ListObjectsLimited(ctx, prefix, limit)
				results <- listResult{prefix: prefix, objects: listed, complete: complete, err: err}
			}
		}()
	}
	go func() {
		for _, prefix := range prefixes {
			jobs <- prefix
		}
		close(jobs)
		workers.Wait()
		close(results)
	}()

	var firstListErr error
	for result := range results {
		stats.ListPrefixes++
		if result.err != nil {
			stats.ListFailedPrefixes++
			if firstListErr == nil {
				firstListErr = result.err
			}
			continue
		}
		stats.ListedObjects += len(result.objects)
		if result.complete {
			stats.ListCompletePrefixes++
		} else {
			stats.ListTruncatedPrefixes++
		}

		targets := make(map[string]struct{}, len(groups[result.prefix]))
		for _, object := range groups[result.prefix] {
			targets[object.key] = struct{}{}
		}
		for _, listed := range result.objects {
			if _, ok := targets[listed.Key]; ok {
				presence[listed.Key] = sourcePresencePresent
			}
		}
		if result.complete {
			for _, object := range groups[result.prefix] {
				if presence[object.key] == sourcePresenceUnknown {
					presence[object.key] = sourcePresenceAbsent
				}
			}
		}
	}
	if firstListErr != nil {
		slog.Warn("source cache prefix listing failed; falling back to object checks",
			"component", "sync", "failed_prefixes", stats.ListFailedPrefixes, "error", firstListErr)
	}
}

func sourceObjectListPrefix(key string) (string, bool) {
	var namespace string
	switch {
	case strings.HasPrefix(key, "assets/"):
		namespace = "assets/"
	case strings.HasPrefix(key, "sources/"):
		namespace = "sources/"
	default:
		return "", false
	}
	remainder := strings.TrimPrefix(key, namespace)
	if remainder == "" || !strings.Contains("0123456789abcdef", remainder[:1]) {
		return "", false
	}
	return namespace + remainder[:1], true
}

func checkSourceObjectsConcurrently(ctx context.Context, store sourceObjectStore, objects []sourceUploadObject, concurrency int) (map[string]bool, int, error) {
	presence := make(map[string]bool, len(objects))
	if len(objects) == 0 {
		return presence, 0, nil
	}
	concurrency = max(1, min(concurrency, len(objects)))
	checkCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type checkResult struct {
		object sourceUploadObject
		exists bool
		err    error
	}
	jobs := make(chan sourceUploadObject)
	results := make(chan checkResult, len(objects))
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for range concurrency {
		go func() {
			defer workers.Done()
			for object := range jobs {
				exists, err := store.ObjectExists(checkCtx, object.key)
				results <- checkResult{object: object, exists: exists, err: err}
				if err != nil {
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, object := range objects {
			select {
			case jobs <- object:
			case <-checkCtx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	requests := 0
	var firstErr error
	for result := range results {
		requests++
		if result.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("check %s exists: %w", result.object.label, result.err)
				cancel()
			}
			continue
		}
		presence[result.object.key] = result.exists
	}
	if firstErr != nil {
		return nil, requests, firstErr
	}
	if len(presence) != len(objects) {
		if err := ctx.Err(); err != nil {
			return nil, requests, err
		}
		return nil, requests, fmt.Errorf("source cache check completed for %d of %d objects", len(presence), len(objects))
	}
	return presence, requests, nil
}

func logSourceUploadStats(ctx context.Context, stats SourceUploadStats) {
	strategy := "head"
	if stats.ReceiptHit {
		strategy = "receipt"
	} else if stats.ListPrefixes > 0 {
		strategy = "list+head"
	}
	level := slog.LevelDebug
	if stats.Objects >= sourceListMinObjectsPerPrefix || stats.DiscoveryDuration >= sourceSlowDiscoveryThreshold || stats.ReceiptLookupFailed || stats.ReceiptWriteFailed || stats.ListFailedPrefixes > 0 {
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, "source R2 cache discovery",
		"component", "sync",
		"strategy", strategy,
		"receipt_hit", stats.ReceiptHit,
		"objects", stats.Objects,
		"cache_hits", stats.CacheHits,
		"cache_misses", stats.CacheMisses,
		"head_requests", stats.HeadRequests,
		"list_prefixes", stats.ListPrefixes,
		"list_complete_prefixes", stats.ListCompletePrefixes,
		"list_truncated_prefixes", stats.ListTruncatedPrefixes,
		"list_failed_prefixes", stats.ListFailedPrefixes,
		"listed_objects", stats.ListedObjects,
		"uploaded_objects", stats.UploadedObjects,
		"hash_ms", stats.HashDuration.Milliseconds(),
		"discovery_ms", stats.DiscoveryDuration.Milliseconds(),
		"upload_ms", stats.UploadDuration.Milliseconds(),
	)
}

func openSourceBlobSnapshot(filename, expectedHash string) (*os.File, func(), error) {
	src, err := os.Open(filename)
	if err != nil {
		return nil, nil, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp("", "weft-source-blob-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		tmp.Close()
		cleanup()
		return nil, nil, err
	}
	actualHash := hex.EncodeToString(h.Sum(nil))
	if actualHash != expectedHash {
		tmp.Close()
		cleanup()
		return nil, nil, fmt.Errorf("source changed while snapshotting: SHA-256 is %s, want %s", actualHash, expectedHash)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		cleanup()
		return nil, nil, err
	}
	return tmp, cleanup, nil
}

func stageSourceDirWithLocalInputs(localDir string, inputs []string) (string, []string, func(), error) {
	return stageSourceDirWithLocalInputsLimit(localDir, inputs, MaxSourceTarballBytes)
}

func stageSourceDirWithLocalInputsLimit(localDir string, inputs []string, maxBytes int64) (string, []string, func(), error) {
	localDir, err := filepath.Abs(localDir)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve source directory: %w", err)
	}
	overlays, err := LocalInputOverlays(localDir, inputs, RequireLocalInput)
	if err != nil {
		return "", nil, nil, err
	}
	if len(overlays) == 0 {
		return "", nil, func() {}, nil
	}
	names := make([]string, len(overlays))
	for i, o := range overlays {
		names[i] = o.Input
	}
	dir, cleanup, err := buildSourceSnapshotWithOverlaysLimit(localDir, overlays, maxBytes)
	return dir, names, cleanup, err
}
