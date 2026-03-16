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

func TestProbeCacheSizesForEnv_UsesHFHubCache(t *testing.T) {
	home := t.TempDir()
	hfHubCache := filepath.Join(home, "custom-hf-hub")
	uvCache := filepath.Join(home, ".cache", "uv")
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
	})
	if probe.HFBytes != 11 {
		t.Fatalf("HFBytes = %d, want 11", probe.HFBytes)
	}
	if probe.UVBytes != 7 {
		t.Fatalf("UVBytes = %d, want 7", probe.UVBytes)
	}
}
