package timeseriescache

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const defaultMaxBytes int64 = 2 << 30

func cacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "weft", "timeseries")
}

// Path returns the local cache path for one raw timeseries object. The etag
// suffix keeps stale data from being reused after an object rewrite.
func Path(jobID, runID int64, etag string) string {
	if etag == "" {
		etag = "unknown"
	}
	return filepath.Join(cacheDir(), fmt.Sprintf("%d-%d-%s.jsonl", jobID, runID, sanitize(etag)))
}

func Read(jobID, runID int64, etag string) ([]byte, bool) {
	path := Path(jobID, runID, etag)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return data, true
}

func Write(jobID, runID int64, etag string, data []byte) error {
	dir := cacheDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := Path(jobID, runID, etag)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return Prune(defaultMaxBytes)
}

func Prune(maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	entries, err := os.ReadDir(cacheDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	type item struct {
		path    string
		size    int64
		modTime time.Time
	}
	items := make([]item, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		it := item{path: filepath.Join(cacheDir(), entry.Name()), size: info.Size(), modTime: info.ModTime()}
		items = append(items, it)
		total += it.size
	}
	for total > maxBytes && len(items) > 0 {
		oldest := 0
		for i := 1; i < len(items); i++ {
			if items[i].modTime.Before(items[oldest].modTime) {
				oldest = i
			}
		}
		_ = os.Remove(items[oldest].path)
		total -= items[oldest].size
		items = append(items[:oldest], items[oldest+1:]...)
	}
	return nil
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
