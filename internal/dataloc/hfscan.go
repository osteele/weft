package dataloc

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
	// Try GNU du -sb (bytes) first; fall back to BSD du -sk (1K blocks) with
	// awk conversion; last resort ls -1d (no size).
	cmd := ResolveHFCacheDirShellVar() + `
_dirs=()
for _p in "$_hf_cache"/models--* "$_hf_cache"/datasets--*; do [ -d "$_p" ] && _dirs+=("$_p"); done
[ ${#_dirs[@]} -eq 0 ] && exit 0
_out=$(du -sb "${_dirs[@]}" 2>/dev/null)
if [ -n "$_out" ]; then
  printf '%s\n' "$_out"
else
  _out=$(du -sk "${_dirs[@]}" 2>/dev/null)
  if [ -n "$_out" ]; then
    printf '%s\n' "$_out" | awk '{printf "%d\t%s\n", $1*1024, $2}'
  else
    printf '%s\n' "${_dirs[@]}"
  fi
fi`
	stdout, _, err := hostCommandRunner(context.Background(), host, cmd)
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

		path, sizeBytes := parseDuLine(line)

		// Extract directory name from path
		parts := strings.Split(path, "/")
		dirName := parts[len(parts)-1]

		asset, ok := ParseHFDirName(dirName)
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

// parseDuLine parses a line of du output in "<bytes>\t<path>" format.
// Falls back to treating the whole line as a plain path with size 0.
func parseDuLine(line string) (path string, sizeBytes int64) {
	if idx := strings.IndexByte(line, '\t'); idx > 0 {
		sizeBytes, _ = strconv.ParseInt(line[:idx], 10, 64)
		path = strings.TrimSpace(line[idx+1:])
	} else {
		path = line
	}
	return
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

		if asset, ok := ParseHFDirName(dirName); ok {
			assets = append(assets, asset)
		}
	}
	return assets
}

// ParseHFDirName parses a HuggingFace cache directory name like
// "models--meta-llama--Llama-3-8B" into a DataAsset.
func ParseHFDirName(name string) (DataAsset, bool) {
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
