package dataloc

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ErrHFAuthRequired is returned when the HF API requires authentication.
var ErrHFAuthRequired = errors.New("HF API auth required")

// ErrHFNoFiles is returned when the HF API returns no files for a model.
var ErrHFNoFiles = errors.New("HF API returned no files")

var ErrHFModelNotFound = errors.New("HF model not found")

var ErrHFRefIsDataset = errors.New("HF ref resolves to a dataset, not a model; use hf-dataset: prefix")

// ErrHFInvalidModelID is returned when a hf: ref's id is not a structurally
// valid HuggingFace model id (e.g. "hf:gpt2 9", with embedded whitespace).
// Such refs never correspond to a real download, so the estimator must not
// apply the unknown-model disk fallback to them.
var ErrHFInvalidModelID = errors.New("HF ref is not a valid model id")

var fetchHFDatasetURL = "https://huggingface.co/api/datasets/%s"

// hfModelSizeCache caches model sizes in-process to avoid redundant API calls
// within an instance launch (multiple jobs may reference the same model).
var hfModelSizeCache sync.Map // map[string]int64

// hfHTTPClient is the HTTP client used for HF API calls (overridable for testing).
var hfHTTPClient = &http.Client{Timeout: 15 * time.Second}

// fetchHFModelSizeURL is the URL template for the HF API (overridable for testing).
var fetchHFModelSizeURL = "https://huggingface.co/api/models/%s/tree/main"

// diskCachePath returns the path to the persistent model size cache.
func diskCachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "weft", "hf-model-sizes.json")
}

// diskCacheOnce ensures the persistent cache is loaded exactly once per process.
var diskCacheOnce sync.Once

// loadDiskCache loads the persistent cache into the in-memory sync.Map.
// It is safe to call from multiple goroutines.
func loadDiskCache() {
	diskCacheOnce.Do(func() {
		path := diskCachePath()
		if path == "" {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var entries map[string]int64
		if err := json.Unmarshal(data, &entries); err != nil {
			return
		}
		for k, v := range entries {
			hfModelSizeCache.LoadOrStore(k, v)
		}
	})
}

// saveDiskCache writes the in-memory cache to disk.
var diskCacheMu sync.Mutex

func saveDiskCache() {
	diskCacheMu.Lock()
	defer diskCacheMu.Unlock()

	path := diskCachePath()
	if path == "" {
		return
	}
	entries := make(map[string]int64)
	hfModelSizeCache.Range(func(key, value any) bool {
		entries[key.(string)] = value.(int64)
		return true
	})
	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// LookupCachedModelSize returns the cached size for a model without making API
// calls. It checks the in-memory cache and disk cache only. Returns (0, false)
// if the model size is not cached.
func LookupCachedModelSize(modelID string) (int64, bool) {
	loadDiskCache()
	if v, ok := hfModelSizeCache.Load(modelID); ok {
		return v.(int64), true
	}
	return 0, false
}

// FetchHFModelSize queries the HuggingFace tree API and sums file sizes.
// Returns total size in bytes for the model's main branch.
func FetchHFModelSize(modelID string) (int64, error) {
	loadDiskCache()

	if cached, ok := hfModelSizeCache.Load(modelID); ok {
		return cached.(int64), nil
	}

	url := fmt.Sprintf(fetchHFModelSizeURL, modelID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build HF API request for %s: %w", modelID, err)
	}
	if token := os.Getenv("HF_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := hfHTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch HF model info for %s: %w", modelID, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return 0, fmt.Errorf("%w for %s; set HF_TOKEN", ErrHFAuthRequired, modelID)
	case http.StatusNotFound:
		return 0, fmt.Errorf("%w: %s", ErrHFModelNotFound, modelID)
	default:
		return 0, fmt.Errorf("HF API returned %d for model %s", resp.StatusCode, modelID)
	}

	var files []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
		LFS  *struct {
			Size int64 `json:"size"`
		} `json:"lfs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return 0, fmt.Errorf("decode HF API response for %s: %w", modelID, err)
	}

	if len(files) == 0 {
		return 0, fmt.Errorf("%w for model %s", ErrHFNoFiles, modelID)
	}

	var totalSize int64
	for _, f := range files {
		// Skip native-checkpoint files that transformers/vLLM never load (e.g.
		// Meta llama original/consolidated.*.pth). Counting them inflates the
		// disk estimate and they are also excluded from the actual download.
		if isRedundantHFModelPath(f.Path) {
			continue
		}
		fileSize := f.Size
		if f.LFS != nil && f.LFS.Size > 0 {
			fileSize = f.LFS.Size
		}
		if fileSize > 0 {
			totalSize += fileSize
		}
	}
	if totalSize <= 0 {
		return 0, fmt.Errorf("HF API returned zero total size for model %s", modelID)
	}

	hfModelSizeCache.Store(modelID, totalSize)
	saveDiskCache()
	return totalSize, nil
}

// PrefetchInputSizes resolves model sizes for all inputs in parallel,
// populating the cache so subsequent ResolveInputSizes calls are fast.
// If onProgress is non-nil, it is called with (resolved, total) after each model.
func PrefetchInputSizes(inputs []string, onProgress func(resolved, total int)) {
	loadDiskCache()

	seen := make(map[string]bool)
	var assets []DataAsset
	for _, input := range inputs {
		asset, ok := ParseAssetRef(input)
		if !ok || asset.Kind != AssetHFModel {
			continue
		}
		if seen[asset.ID] {
			continue
		}
		seen[asset.ID] = true
		if _, ok := hfModelSizeCache.Load(asset.ID); ok {
			continue
		}
		assets = append(assets, asset)
	}
	if len(assets) == 0 {
		return
	}

	var resolved atomic.Int32
	total := len(assets)
	if onProgress != nil {
		onProgress(0, total)
	}

	var wg sync.WaitGroup
	for _, asset := range assets {
		wg.Add(1)
		go func(a DataAsset) {
			defer wg.Done()
			_, _ = FetchHFModelSize(a.ID)
			cur := int(resolved.Add(1))
			if onProgress != nil {
				onProgress(cur, total)
			}
		}(asset)
	}
	wg.Wait()
	saveDiskCache()
}

// ResolveInputSizes computes the total size in bytes of all sized asset refs.
// Per-ref failures are non-fatal: the total is the sum of successfully
// resolved refs, unresolved lists the original input strings that failed,
// and err wraps the individual errors. Callers needing only a best-effort
// total can ignore err and unresolved.
func ResolveInputSizes(inputs []string, localDB *sql.DB) (totalBytes int64, unresolved []string, err error) {
	seen := make(map[string]bool)
	var errs []error

	for _, input := range inputs {
		asset, ok := ParseAssetRef(input)
		if !ok {
			continue
		}
		key := asset.Ref()
		if seen[key] {
			continue
		}
		seen[key] = true

		switch asset.Kind {
		case AssetHFModel:
			size, resolveErr := resolveModelSize(asset, localDB)
			if resolveErr == nil {
				totalBytes += size
				continue
			}
			// The ref did not resolve. A structurally invalid ref (e.g.
			// "hf:gpt2 9") is a phantom that never downloads, so it must not
			// receive the unknown-model disk fallback that EstimateGroupDisk
			// applies per unresolved ref. A plausible model that is merely
			// unreachable (gated/private), or a dataset mis-prefixed as a model,
			// keeps the fallback — both may correspond to a real download, so
			// over-provisioning is the safe choice.
			if !IsHFModelID(asset.ID) {
				errs = append(errs, fmt.Errorf("%s: %w", input, ErrHFInvalidModelID))
				continue
			}
			unresolved = append(unresolved, input)
			errs = append(errs, fmt.Errorf("%s: %w", input, resolveErr))
		case AssetCorpus:
			totalBytes += resolveAssetSizeFromDB(asset, localDB)
		}
	}

	if len(errs) > 0 {
		err = errors.Join(errs...)
	}
	return totalBytes, unresolved, err
}

// resolveAssetSizeFromDB looks up the size of an asset from the host_data table.
// Returns 0 if the asset is not found or has no recorded size.
func resolveAssetSizeFromDB(asset DataAsset, localDB *sql.DB) int64 {
	if localDB == nil {
		return 0
	}
	entries, err := FindAssetHosts(localDB, asset)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.SizeBytes > 0 {
			return e.SizeBytes
		}
	}
	return 0
}

// resolveModelSize returns the size of a single model, checking the local DB
// first and falling back to the HF API. A 404 that also matches the datasets
// API is wrapped as ErrHFRefIsDataset so the caller can nudge the user to
// the correct hf-dataset: prefix.
func resolveModelSize(asset DataAsset, localDB *sql.DB) (int64, error) {
	dbSize := resolveAssetSizeFromDB(asset, localDB)
	hfSize, err := FetchHFModelSize(asset.ID)
	if err != nil && errors.Is(err, ErrHFModelNotFound) && hfRefIsDataset(asset.ID) {
		return 0, fmt.Errorf("%w: %s (did you mean hf-dataset:%s?)", ErrHFRefIsDataset, asset.ID, asset.ID)
	}
	if err != nil {
		if dbSize > 0 {
			return dbSize, nil
		}
		return 0, err
	}
	if dbSize > hfSize {
		return dbSize, nil
	}
	return hfSize, nil
}

// hfDatasetExistsCache memoises hfRefIsDataset probes for this process so
// repeated unresolved refs across a campaign don't each pay an HF API call.
var hfDatasetExistsCache sync.Map // map[string]bool

// hfRefIsDataset returns true if the given ref exists as a dataset on HF.
// Network/API errors are treated as false — the caller falls back to the
// original model-not-found error.
func hfRefIsDataset(ref string) bool {
	if v, ok := hfDatasetExistsCache.Load(ref); ok {
		return v.(bool)
	}
	url := fmt.Sprintf(fetchHFDatasetURL, ref)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	if token := os.Getenv("HF_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hfHTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	exists := resp.StatusCode == http.StatusOK
	hfDatasetExistsCache.Store(ref, exists)
	return exists
}

// NormalizeMisprefixedHFDatasets rewrites `hf:X` input refs to `hf-dataset:X`
// when X exists as a HuggingFace dataset — a common declaration error, since
// datasets are not models, that otherwise causes weft to stage the ref as a
// model (which fails) and the job to fetch the dataset live at runtime. Returns
// the normalized inputs and the original refs that were corrected (for caller
// warnings). Refs already shadowed by an explicit `hf-dataset:X` are left for
// the caller's existing dedup. The dataset probe is memoized and fails safe
// (a network error leaves the ref unchanged).
func NormalizeMisprefixedHFDatasets(inputs []string) (out, corrected []string) {
	hasDatasetForm := make(map[string]bool)
	for _, in := range inputs {
		if a, ok := ParseAssetRef(in); ok && a.Kind == AssetHFDataset {
			hasDatasetForm[a.ID] = true
		}
	}
	out = make([]string, 0, len(inputs))
	for _, in := range inputs {
		a, ok := ParseAssetRef(in)
		if ok && a.Kind == AssetHFModel && !hasDatasetForm[a.ID] && hfRefIsDataset(a.ID) {
			out = append(out, DataAsset{Kind: AssetHFDataset, ID: a.ID}.Ref())
			corrected = append(corrected, in)
			hasDatasetForm[a.ID] = true
			continue
		}
		out = append(out, in)
	}
	return out, corrected
}

// ClearHFModelSizeCache clears the in-process model size cache (for testing).
func ClearHFModelSizeCache() {
	hfModelSizeCache = sync.Map{}
	hfDatasetExistsCache = sync.Map{}
	diskCacheOnce = sync.Once{}
}

// OverrideHFURLsForTesting swaps the HF API URL templates and HTTP client to
// point at a test server. Returns a restore function the caller should defer.
// Also clears the in-memory size cache so stale values don't bleed between
// tests.
func OverrideHFURLsForTesting(baseURL string, client *http.Client) (restore func()) {
	ClearHFModelSizeCache()
	origClient := hfHTTPClient
	origModelURL := fetchHFModelSizeURL
	origDatasetURL := fetchHFDatasetURL
	if client != nil {
		hfHTTPClient = client
	}
	fetchHFModelSizeURL = baseURL + "/api/models/%s/tree/main"
	fetchHFDatasetURL = baseURL + "/api/datasets/%s"
	return func() {
		hfHTTPClient = origClient
		fetchHFModelSizeURL = origModelURL
		fetchHFDatasetURL = origDatasetURL
	}
}
