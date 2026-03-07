package dataloc

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// hfModelSizeCache caches model sizes in-process to avoid redundant API calls
// within a campaign launch (multiple jobs may reference the same model).
var hfModelSizeCache sync.Map // map[string]int64

// hfHTTPClient is the HTTP client used for HF API calls (overridable for testing).
var hfHTTPClient = &http.Client{Timeout: 15 * time.Second}

// fetchHFModelSizeURL is the URL template for the HF API (overridable for testing).
var fetchHFModelSizeURL = "https://huggingface.co/api/models/%s"

// FetchHFModelSize queries the HuggingFace API for a model's total storage size.
// Returns size in bytes from the usedStorage field.
func FetchHFModelSize(modelID string) (int64, error) {
	if cached, ok := hfModelSizeCache.Load(modelID); ok {
		return cached.(int64), nil
	}

	url := fmt.Sprintf(fetchHFModelSizeURL, modelID)
	resp, err := hfHTTPClient.Get(url)
	if err != nil {
		return 0, fmt.Errorf("fetch HF model info for %s: %w", modelID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HF API returned %d for model %s", resp.StatusCode, modelID)
	}

	var result struct {
		UsedStorage int64 `json:"usedStorage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode HF API response for %s: %w", modelID, err)
	}

	if result.UsedStorage <= 0 {
		return 0, fmt.Errorf("HF API returned no usedStorage for model %s", modelID)
	}

	hfModelSizeCache.Store(modelID, result.UsedStorage)
	return result.UsedStorage, nil
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
}
