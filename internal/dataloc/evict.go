package dataloc

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HostDataEntryWithAtime extends HostDataEntry with filesystem access time.
type HostDataEntryWithAtime struct {
	HostDataEntry
	LastAccessed time.Time
}

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
		asset, ok := parseHFDirName(dirName)
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
