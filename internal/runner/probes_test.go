package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHFCacheProbeDir_Default(t *testing.T) {
	home := t.TempDir()
	got := resolveHFCacheProbeDir([]string{"HOME=" + home}, home)
	want := filepath.Join(home, ".cache", "huggingface", "hub")
	if got != want {
		t.Fatalf("resolveHFCacheProbeDir() = %q, want %q", got, want)
	}
}

func TestResolveHFCacheProbeDir_HFHome(t *testing.T) {
	home := t.TempDir()
	got := resolveHFCacheProbeDir([]string{"HOME=" + home, "HF_HOME=~/hf-home"}, home)
	want := filepath.Join(home, "hf-home", "hub")
	if got != want {
		t.Fatalf("resolveHFCacheProbeDir() = %q, want %q", got, want)
	}
}

func TestResolveHFCacheProbeDir_HFHubCacheTakesPrecedence(t *testing.T) {
	home := t.TempDir()
	got := resolveHFCacheProbeDir([]string{
		"HOME=" + home,
		"HF_HOME=" + filepath.Join(home, "hf-home"),
		"HF_HUB_CACHE=~/hub-cache",
	}, home)
	want := filepath.Join(home, "hub-cache")
	if got != want {
		t.Fatalf("resolveHFCacheProbeDir() = %q, want %q", got, want)
	}
}

func TestResolveUVCacheProbeDir(t *testing.T) {
	home := t.TempDir()

	got := resolveUVCacheProbeDir([]string{"HOME=" + home, "UV_CACHE_DIR=~/uv-cache"}, home)
	want := filepath.Join(home, "uv-cache")
	if got != want {
		t.Fatalf("resolveUVCacheProbeDir(UV_CACHE_DIR) = %q, want %q", got, want)
	}

	got = resolveUVCacheProbeDir([]string{"HOME=" + home, "XDG_CACHE_HOME=~/xdg-cache"}, home)
	want = filepath.Join(home, "xdg-cache", "uv")
	if got != want {
		t.Fatalf("resolveUVCacheProbeDir(XDG_CACHE_HOME) = %q, want %q", got, want)
	}
}

func TestResolveDiskProbePathPrefersExplicitPath(t *testing.T) {
	home := t.TempDir()
	workDir := filepath.Join(home, "work")
	got := resolveDiskProbePath([]string{
		"HOME=" + home,
		"UV_CACHE_DIR=" + filepath.Join(home, "uv-cache"),
	}, home, workDir)
	if got != workDir {
		t.Fatalf("resolveDiskProbePath() = %q, want %q", got, workDir)
	}
}

func TestResolveDiskProbePathFallsBackToUVCache(t *testing.T) {
	home := t.TempDir()
	uvCache := filepath.Join(home, "uv-cache")
	got := resolveDiskProbePath([]string{
		"HOME=" + home,
		"UV_CACHE_DIR=" + uvCache,
	}, home, "")
	if got != uvCache {
		t.Fatalf("resolveDiskProbePath() = %q, want %q", got, uvCache)
	}
}

func TestProbeCacheSizesForEnv_UsesConfiguredCaches(t *testing.T) {
	home := t.TempDir()
	hfHubCache := filepath.Join(home, "custom-hf-hub")
	uvCache := filepath.Join(home, "custom-uv")
	if err := os.MkdirAll(hfHubCache, 0o755); err != nil {
		t.Fatalf("mkdir hf cache: %v", err)
	}
	if err := os.MkdirAll(uvCache, 0o755); err != nil {
		t.Fatalf("mkdir uv cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hfHubCache, "weights.bin"), make([]byte, 11), 0o644); err != nil {
		t.Fatalf("write hf cache file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(uvCache, "wheel.whl"), make([]byte, 7), 0o644); err != nil {
		t.Fatalf("write uv cache file: %v", err)
	}

	probe := ProbeCacheSizesForEnv([]string{
		"HOME=" + home,
		"HF_HOME=" + filepath.Join(home, "ignored-hf-home"),
		"HF_HUB_CACHE=" + hfHubCache,
		"UV_CACHE_DIR=" + uvCache,
	})
	if probe.HFBytes != 11 {
		t.Fatalf("HFBytes = %d, want 11", probe.HFBytes)
	}
	if probe.UVBytes != 7 {
		t.Fatalf("UVBytes = %d, want 7", probe.UVBytes)
	}
}
