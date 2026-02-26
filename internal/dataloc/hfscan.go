package dataloc

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/ssh"
)

// ScanHFCache scans the HuggingFace cache on a remote host and returns
// discovered models and datasets. It looks for directories matching the
// HF cache layout: ~/.cache/huggingface/hub/models--<org>--<name>
// and ~/.cache/huggingface/hub/datasets--<org>--<name>.
func ScanHFCache(host string) ([]DataAsset, error) {
	entries, err := ScanHFCacheDetailed(host)
	if err != nil {
		return nil, err
	}
	assets := make([]DataAsset, len(entries))
	for i, e := range entries {
		assets[i] = e.Asset
	}
	return assets, nil
}

// ScanHFCacheDetailed scans the HuggingFace cache on a remote host and returns
// entries with full path and size information for each discovered asset.
func ScanHFCacheDetailed(host string) ([]HostDataEntry, error) {
	// Use du -sb to get both size and path in one command.
	// du -sb outputs: <bytes>\t<path>
	// Falls back to ls -1d if du fails (e.g., macOS without coreutils).
	cmd := `du -sb ~/.cache/huggingface/hub/models--* ~/.cache/huggingface/hub/datasets--* 2>/dev/null || ls -1d ~/.cache/huggingface/hub/models--* ~/.cache/huggingface/hub/datasets--* 2>/dev/null || true`
	stdout, _, err := ssh.Run(host, cmd)
	if err != nil {
		return nil, fmt.Errorf("scan HF cache on %s: %w", host, err)
	}
	return parseHFCacheDetailedOutput(stdout, host), nil
}

// parseHFCacheDetailedOutput parses output from du -sb or ls -1d into HostDataEntries.
// du -sb format: "<bytes>\t<path>"
// ls -1d format: "<path>"
func parseHFCacheDetailedOutput(output string, host string) []HostDataEntry {
	var entries []HostDataEntry
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var path string
		var sizeBytes int64

		// Try du -sb format: "<bytes>\t<path>"
		if idx := strings.IndexByte(line, '\t'); idx > 0 {
			sizeStr := line[:idx]
			path = strings.TrimSpace(line[idx+1:])
			sizeBytes, _ = strconv.ParseInt(sizeStr, 10, 64)
		} else {
			// Fallback: plain path from ls -1d
			path = line
		}

		// Extract directory name from path
		parts := strings.Split(path, "/")
		dirName := parts[len(parts)-1]

		asset, ok := parseHFDirName(dirName)
		if !ok {
			continue
		}

		entries = append(entries, HostDataEntry{
			Host:      host,
			Asset:     asset,
			Path:      path,
			SizeBytes: sizeBytes,
		})
	}
	return entries
}

// parseHFCacheOutput parses `ls` output of HF cache directories into DataAssets.
func parseHFCacheOutput(output string) []DataAsset {
	var assets []DataAsset
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Extract the directory name from the path
		parts := strings.Split(line, "/")
		dirName := parts[len(parts)-1]

		if asset, ok := parseHFDirName(dirName); ok {
			assets = append(assets, asset)
		}
	}
	return assets
}

// parseHFDirName parses a HuggingFace cache directory name like
// "models--meta-llama--Llama-3-8B" into a DataAsset.
func parseHFDirName(name string) (DataAsset, bool) {
	if strings.HasPrefix(name, "models--") {
		id := strings.TrimPrefix(name, "models--")
		id = hfDirToID(id)
		return DataAsset{Kind: AssetHFModel, ID: id}, true
	}
	if strings.HasPrefix(name, "datasets--") {
		id := strings.TrimPrefix(name, "datasets--")
		id = hfDirToID(id)
		return DataAsset{Kind: AssetHFDataset, ID: id}, true
	}
	return DataAsset{}, false
}

// hfDirToID converts a HF cache directory name component to a HF ID.
// HF uses "--" as separator in cache dir names: "meta-llama--Llama-3-8B" -> "meta-llama/Llama-3-8B"
func hfDirToID(dirPart string) string {
	// Replace the first "--" with "/", which separates org from model name.
	// Subsequent "--" are not expected in standard HF naming.
	return strings.Replace(dirPart, "--", "/", 1)
}
