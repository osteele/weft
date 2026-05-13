package dataloc

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HostDataEntryWithAtime extends HostDataEntry with filesystem access time.
type HostDataEntryWithAtime struct {
	HostDataEntry
	LastAccessed time.Time
}

const bytesPerGB = 1_000_000_000

// ScanHFCacheWithAtime scans the HF cache on a remote host and returns entries
// with last-access times from the filesystem. Respects HF_HUB_CACHE and HF_HOME.
// Uses find -printf (GNU, Linux) with a shell loop fallback (BSD/macOS).
func ScanHFCacheWithAtime(host string) ([]HostDataEntryWithAtime, error) {
	// find -printf '%A@\t%s\t%p\n' gives: atime_float\tsize_bytes\tpath (GNU find, Linux)
	// Fallback: shell loop using stat, works on macOS.
	cmd := ResolveHFCacheDirShellVar() + `
if find "$_hf_cache" -maxdepth 1 -printf '' 2>/dev/null; then
  find "$_hf_cache" -maxdepth 1 \( -name 'models--*' -o -name 'datasets--*' \) \
    -type d -printf '%A@\t%s\t%p\n' 2>/dev/null
else
  for p in "$_hf_cache"/models--* "$_hf_cache"/datasets--*; do
    [ -d "$p" ] || continue
    atime=$(stat -f "%Xa" "$p" 2>/dev/null || stat -c "%X" "$p" 2>/dev/null || echo 0)
    size=$(du -sb "$p" 2>/dev/null | cut -f1 || echo 0)
    printf '%s\t%s\t%s\n' "$atime" "$size" "$p"
  done
fi`
	stdout, _, err := hostCommandRunner(context.Background(), host, cmd)
	if err != nil {
		return nil, fmt.Errorf("scan HF cache atimes on %s: %w", host, err)
	}
	return parseHFCacheAtimeOutput(stdout, host), nil
}

func parseHFCacheAtimeOutput(output, host string) []HostDataEntryWithAtime {
	var entries []HostDataEntryWithAtime
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		atimeStr, sizeStr, path := parts[0], parts[1], parts[2]

		// atime may be a float (find -printf '%A@') or integer (stat %X)
		atimeSec, err := strconv.ParseFloat(strings.TrimSpace(atimeStr), 64)
		if err != nil {
			continue
		}
		sizeBytes, _ := strconv.ParseInt(strings.TrimSpace(sizeStr), 10, 64)

		dirName := filepath.Base(path)
		asset, ok := ParseHFDirName(dirName)
		if !ok {
			continue
		}

		entries = append(entries, HostDataEntryWithAtime{
			HostDataEntry: HostDataEntry{
				Host:      host,
				Asset:     asset,
				Path:      path,
				SizeBytes: sizeBytes,
			},
			LastAccessed: time.Unix(int64(atimeSec), 0),
		})
	}
	return entries
}

// RecentAssetUseCounts returns per-candidate job-input use counts since the
// cutoff. Counts are scoped to the asset's host.
func RecentAssetUseCounts(db *sql.DB, entries []HostDataEntryWithUsage, since time.Time) (map[string]int, error) {
	counts := make(map[string]int, len(entries))
	for _, entry := range entries {
		ref := assetInputRef(entry.Asset)
		if ref == "" {
			continue
		}
		var count int
		err := db.QueryRow(`
			SELECT COUNT(*)
			FROM jobs j, json_each(j.inputs) je
			WHERE j.host = ?
			  AND j.start_time >= ?
			  AND j.inputs IS NOT NULL
			  AND je.value = ?
		`, entry.Host, since.Unix(), ref).Scan(&count)
		if err != nil {
			return nil, err
		}
		counts[evictionEntryKey(entry)] = count
	}
	return counts, nil
}

func assetInputRef(asset DataAsset) string {
	switch asset.Kind {
	case AssetHFModel:
		return "hf:" + asset.ID
	case AssetHFDataset:
		return "hf-dataset:" + asset.ID
	case AssetCheckpoint:
		return "checkpoint:" + asset.ID
	case AssetJobOutput:
		return "job-output:" + asset.ID
	default:
		return ""
	}
}

// SortEvictionCandidates orders entries for eviction. The default "lru" policy
// evicts least-recently used entries first. "reuse_per_gb" evicts the lowest
// recent-use density first, using recentCounts from RecentAssetUseCounts.
func SortEvictionCandidates(entries []HostDataEntryWithUsage, policy string, recentCounts map[string]int) {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "reuse_per_gb":
		sort.SliceStable(entries, func(i, j int) bool {
			di := reusePerGBDensity(entries[i], recentCounts)
			dj := reusePerGBDensity(entries[j], recentCounts)
			if di != dj {
				return di < dj
			}
			return lruLess(entries[i], entries[j])
		})
	default:
		sort.SliceStable(entries, func(i, j int) bool {
			return lruLess(entries[i], entries[j])
		})
	}
}

func reusePerGBDensity(entry HostDataEntryWithUsage, counts map[string]int) float64 {
	sizeGB := float64(entry.SizeBytes) / bytesPerGB
	if sizeGB <= 0 {
		sizeGB = 1
	}
	return float64(counts[evictionEntryKey(entry)]) / sizeGB
}

func lruLess(a, b HostDataEntryWithUsage) bool {
	if !a.LastUsedAt.Equal(b.LastUsedAt) {
		return a.LastUsedAt.Before(b.LastUsedAt)
	}
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	if a.Asset.Kind != b.Asset.Kind {
		return a.Asset.Kind < b.Asset.Kind
	}
	return a.Asset.ID < b.Asset.ID
}

func evictionEntryKey(entry HostDataEntryWithUsage) string {
	return entry.Host + "\x00" + string(entry.Asset.Kind) + "\x00" + entry.Asset.ID
}

// DeleteHostDataEntry removes a single asset entry from the local inventory DB.
func DeleteHostDataEntry(db *sql.DB, host string, asset DataAsset) error {
	_, err := db.Exec(`
		DELETE FROM host_data WHERE host = ? AND asset_kind = ? AND asset_id = ?
	`, host, string(asset.Kind), asset.ID)
	return err
}

// EvictAsset deletes an HF cache directory on a remote host.
// path must be the absolute path returned by ScanHFCacheWithAtime.
func EvictAsset(host, path string) error {
	if path == "" || path == "/" {
		return fmt.Errorf("refusing to delete empty or root path")
	}
	// Sanity check: path must look like an HF cache dir
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "models--") && !strings.HasPrefix(base, "datasets--") {
		return fmt.Errorf("refusing to delete non-HF path %q", path)
	}
	cmd := fmt.Sprintf("rm -rf %q", path)
	_, _, err := hostCommandRunner(context.Background(), host, cmd)
	return err
}
