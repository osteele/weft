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
	return ProbeCacheSizesForEnvAtPath(env, "")
}

// ProbeCacheSizesForEnvAtPath measures cache sizes and disk usage for the
// filesystem that contains diskPath. If diskPath is empty, it uses the cache or
// home path from env.
func ProbeCacheSizesForEnvAtPath(env []string, diskPath string) CacheProbe {
	home := envValue(env, "HOME")
	if home == "" {
		home = ExpandTilde("~/")
	}
	uvDir := resolveUVCacheProbeDir(env, home)
	probe := CacheProbe{
		HFBytes: DirSizeBytes(resolveHFCacheProbeDir(env, home)),
		UVBytes: DirSizeBytes(uvDir),
	}
	probe.DiskUsedBytes, probe.DiskTotalBytes = ProbeDiskUsageAtPath(resolveDiskProbePath(env, home, diskPath))
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

func resolveUVCacheProbeDir(env []string, home string) string {
	if cacheDir := envValue(env, "UV_CACHE_DIR"); cacheDir != "" {
		return expandHomePath(cacheDir, home)
	}
	if cacheHome := envValue(env, "XDG_CACHE_HOME"); cacheHome != "" {
		return filepath.Join(expandHomePath(cacheHome, home), "uv")
	}
	return filepath.Join(home, ".cache", "uv")
}

func resolveDiskProbePath(env []string, home, diskPath string) string {
	for _, path := range []string{
		diskPath,
		resolveUVCacheProbeDir(env, home),
		resolveHFCacheProbeDir(env, home),
		home,
	} {
		if strings.TrimSpace(path) != "" {
			return path
		}
	}
	return "/"
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
	return ProbeDiskUsageAtPath("/")
}

// ProbeDiskUsageAtPath returns (used, total) bytes for the filesystem that
// contains path.
func ProbeDiskUsageAtPath(path string) (used, total int64) {
	if path == "" {
		path = "/"
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	bsize := StatfsBlockBytes(stat)
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
