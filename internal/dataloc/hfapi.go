package dataloc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// hfModelSizeCache caches model sizes in-process to avoid redundant API calls
// within a campaign launch (multiple jobs may reference the same model).
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

// diskCacheLoaded tracks whether we've loaded from disk this process.
var diskCacheLoaded bool

// loadDiskCache loads the persistent cache into the in-memory sync.Map.
func loadDiskCache() {
	if diskCacheLoaded {
		return
	}
	diskCacheLoaded = true

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
		return 0, fmt.Errorf("HF API auth required for %s; set HF_TOKEN", modelID)
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
		return 0, fmt.Errorf("HF API returned no files for model %s", modelID)
	}

	var totalSize int64
	for _, f := range files {
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

// ResolveInputSizes computes the total size in bytes of all hf:* model refs
// in the given input list. It first checks the local host_data DB for known sizes,
// falling back to the HF API for unknown models.
// Non-HF inputs are ignored.
func ResolveInputSizes(inputs []string, localDB *sql.DB) (int64, error) {
	seen := make(map[string]bool)
	var totalBytes int64

	for _, input := range inputs {
		asset, ok := ParseAssetRef(input)
		if !ok || asset.Kind != AssetHFModel {
			continue
		}
		if seen[asset.ID] {
			continue
		}
		seen[asset.ID] = true

		size, err := resolveModelSize(asset, localDB)
		if err != nil {
			return totalBytes, err
		}
		totalBytes += size
	}

	return totalBytes, nil
}

// resolveModelSize returns the size of a single model, checking the local DB
// first and falling back to the HF API.
func resolveModelSize(asset DataAsset, localDB *sql.DB) (int64, error) {
	if localDB != nil {
		entries, err := FindAssetHosts(localDB, asset)
		if err == nil {
			for _, e := range entries {
				if e.SizeBytes > 0 {
					return e.SizeBytes, nil
				}
			}
		}
	}
	return FetchHFModelSize(asset.ID)
}

// ClearHFModelSizeCache clears the in-process model size cache (for testing).
func ClearHFModelSizeCache() {
	hfModelSizeCache = sync.Map{}
	diskCacheLoaded = false
}
