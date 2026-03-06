package runner

import (
	"io/fs"
	"path/filepath"
)

// CacheProbe holds cache directory sizes in bytes.
type CacheProbe struct {
	HFBytes int64 `json:"hf_bytes"`
	UVBytes int64 `json:"uv_bytes"`
}

// ProbeCacheSizes measures the sizes of common cache directories.
func ProbeCacheSizes() CacheProbe {
	home := expandTilde("~/")
	return CacheProbe{
		HFBytes: dirSizeBytes(filepath.Join(home, ".cache", "huggingface")),
		UVBytes: dirSizeBytes(filepath.Join(home, ".cache", "uv")),
	}
}

// dirSizeBytes returns the total size of all regular files under dir.
// Returns 0 if the directory does not exist or cannot be read.
func dirSizeBytes(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
