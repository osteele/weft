package runner

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	return ProbeCacheSizesForEnv(os.Environ())
}

// ProbeCacheSizesForEnv measures cache sizes using the provided environment.
// HF_HUB_CACHE takes precedence over HF_HOME, matching huggingface_hub behavior.
func ProbeCacheSizesForEnv(env []string) CacheProbe {
	home := envValue(env, "HOME")
	if home == "" {
		home = ExpandTilde("~/")
	}
	probe := CacheProbe{
		HFBytes: DirSizeBytes(resolveHFCacheProbeDir(env, home)),
		UVBytes: DirSizeBytes(filepath.Join(home, ".cache", "uv")),
	}
	probe.DiskUsedBytes, probe.DiskTotalBytes = ProbeDiskUsage()
	return probe
}

func resolveHFCacheProbeDir(env []string, home string) string {
	if cacheDir := envValue(env, "HF_HUB_CACHE"); cacheDir != "" {
		return expandHomePath(cacheDir, home)
	}
	if hfHome := envValue(env, "HF_HOME"); hfHome != "" {
		return filepath.Join(expandHomePath(hfHome, home), "hub")
	}
	return filepath.Join(home, ".cache", "huggingface", "hub")
}

func expandHomePath(path, home string) string {
	switch {
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:])
	default:
		return path
	}
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

// DirSizeBytes returns the total size of all regular files under dir.
// Returns 0 if the directory does not exist or cannot be read.
func DirSizeBytes(dir string) int64 {
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
