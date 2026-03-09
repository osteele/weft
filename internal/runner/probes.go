package runner

import (
	"io/fs"
	"path/filepath"
	"syscall"
)

// CacheProbe holds cache directory sizes in bytes.
type CacheProbe struct {
	HFBytes        int64 `json:"hf_bytes"`
	UVBytes        int64 `json:"uv_bytes"`
	DiskUsedBytes  int64 `json:"disk_used_bytes,omitempty"`
	DiskTotalBytes int64 `json:"disk_total_bytes,omitempty"`
}

// ProbeCacheSizes measures the sizes of common cache directories and root disk usage.
func ProbeCacheSizes() CacheProbe {
	home := expandTilde("~/")
	probe := CacheProbe{
		HFBytes: dirSizeBytes(filepath.Join(home, ".cache", "huggingface")),
		UVBytes: dirSizeBytes(filepath.Join(home, ".cache", "uv")),
	}
	probe.DiskUsedBytes, probe.DiskTotalBytes = ProbeDiskUsage()
	return probe
}

// ProbeDiskUsage returns (used, total) bytes for the root filesystem.
func ProbeDiskUsage() (used, total int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return 0, 0
	}
	bsize := int64(stat.Bsize)
	total = int64(stat.Blocks) * bsize
	free := int64(stat.Bfree) * bsize
	return total - free, total
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
