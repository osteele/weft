package agentdeploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCachePath(t *testing.T) {
	path := CachePath("abc123def456", "linux", "amd64")

	if !strings.Contains(path, "weft/builds/abc123def456/linux-amd64/weft-agent") {
		t.Errorf("unexpected cache path: %s", path)
	}

	dir := filepath.Dir(path)
	if !strings.HasSuffix(dir, "linux-amd64") {
		t.Errorf("expected dir to end with linux-amd64, got %s", dir)
	}

	base := filepath.Base(path)
	if base != "weft-agent" {
		t.Errorf("expected binary name weft-agent, got %s", base)
	}
}

func TestCachePath_DifferentPlatforms(t *testing.T) {
	linux := CachePath("v1", "linux", "amd64")
	darwin := CachePath("v1", "darwin", "arm64")

	if linux == darwin {
		t.Error("linux and darwin cache paths should differ")
	}
	if !strings.Contains(linux, "linux-amd64") {
		t.Errorf("linux path missing platform: %s", linux)
	}
	if !strings.Contains(darwin, "darwin-arm64") {
		t.Errorf("darwin path missing platform: %s", darwin)
	}
}

func TestCachePath_DifferentVersions(t *testing.T) {
	v1 := CachePath("abc123", "linux", "amd64")
	v2 := CachePath("def456", "linux", "amd64")

	if v1 == v2 {
		t.Error("different versions should produce different cache paths")
	}
	if !strings.Contains(v1, "abc123") {
		t.Errorf("v1 path missing version: %s", v1)
	}
	if !strings.Contains(v2, "def456") {
		t.Errorf("v2 path missing version: %s", v2)
	}
}

func TestCachePath_Structure(t *testing.T) {
	path := CachePath("deadbeef1234", "linux", "amd64")

	// Should be: <cache>/weft/builds/<version>/<os>-<arch>/weft-agent
	parts := strings.Split(path, string(filepath.Separator))

	// Find "weft" in the path
	weftIdx := -1
	for i, p := range parts {
		if p == "weft" {
			weftIdx = i
			break
		}
	}
	if weftIdx < 0 {
		t.Fatalf("path missing 'weft' component: %s", path)
	}
	if parts[weftIdx+1] != "builds" {
		t.Errorf("expected 'builds' after 'weft', got %s", parts[weftIdx+1])
	}
	if parts[weftIdx+2] != "deadbeef1234" {
		t.Errorf("expected version, got %s", parts[weftIdx+2])
	}
	if parts[weftIdx+3] != "linux-amd64" {
		t.Errorf("expected platform, got %s", parts[weftIdx+3])
	}
	if parts[weftIdx+4] != "weft-agent" {
		t.Errorf("expected binary name, got %s", parts[weftIdx+4])
	}
}

func TestEnsureBuilt_CacheHit(t *testing.T) {
	version := "test-cache-hit-" + t.Name()
	path := CachePath(version, "linux", "amd64")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fake-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })

	got, err := EnsureBuilt(version, "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureBuilt: %v", err)
	}
	if got != path {
		t.Errorf("got %s, want %s", got, path)
	}
}

func TestEnsureBuilt_CacheMiss_InvokesBuild(t *testing.T) {
	var captured struct {
		root, ldflags, outputPath, goos, goarch string
	}

	cleanup := SetBuildFunc(func(root, ldflags, outputPath, goos, goarch string) error {
		captured.root = root
		captured.ldflags = ldflags
		captured.outputPath = outputPath
		captured.goos = goos
		captured.goarch = goarch
		// Write a fake binary so EnsureBuilt succeeds
		return os.WriteFile(outputPath, []byte("fake-agent"), 0o755)
	})
	defer cleanup()

	version := "test-mock-build-" + t.Name()
	path := CachePath(version, "linux", "amd64")
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })

	got, err := EnsureBuilt(version, "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureBuilt: %v", err)
	}
	if got != path {
		t.Errorf("got %s, want %s", got, path)
	}

	// Verify build was called with correct arguments
	if captured.goos != "linux" {
		t.Errorf("GOOS = %q, want %q", captured.goos, "linux")
	}
	if captured.goarch != "amd64" {
		t.Errorf("GOARCH = %q, want %q", captured.goarch, "amd64")
	}
	if !strings.Contains(captured.ldflags, version) {
		t.Errorf("ldflags %q should contain version %q", captured.ldflags, version)
	}
	if captured.outputPath != path {
		t.Errorf("output path = %q, want %q", captured.outputPath, path)
	}
	if captured.root == "" {
		t.Error("root directory should not be empty")
	}
}

func TestEnsureBuilt_CacheHit_SkipsBuild(t *testing.T) {
	// Pre-populate cache with known content
	version := "test-skip-build-" + t.Name()
	path := CachePath(version, "linux", "amd64")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("sentinel-value")
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })

	got, err := EnsureBuilt(version, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}

	// Read the file back — it should still be our sentinel, not a real binary
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "sentinel-value" {
		t.Error("cache hit should return existing file without rebuilding")
	}
}

func TestEnsureBuilt_CrossCompile_CorrectArgs(t *testing.T) {
	var captured struct {
		goos, goarch string
	}

	cleanup := SetBuildFunc(func(root, ldflags, outputPath, goos, goarch string) error {
		captured.goos = goos
		captured.goarch = goarch
		return os.WriteFile(outputPath, []byte("fake-agent"), 0o755)
	})
	defer cleanup()

	version := "test-cross-" + t.Name()
	path := CachePath(version, "linux", "amd64")
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })

	_, err := EnsureBuilt(version, "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureBuilt: %v", err)
	}

	if captured.goos != "linux" {
		t.Errorf("GOOS = %q, want %q", captured.goos, "linux")
	}
	if captured.goarch != "amd64" {
		t.Errorf("GOARCH = %q, want %q", captured.goarch, "amd64")
	}
}

func TestEnsureBuilt_RealBuild_NativePlatform(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow build test in short mode")
	}
	// Build for the native platform (should succeed without cross-compilation issues)
	version := "test-native-build-" + t.Name()
	path := CachePath(version, "darwin", "arm64")
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })

	got, err := EnsureBuilt(version, "darwin", "arm64")
	if err != nil {
		t.Fatalf("EnsureBuilt native: %v", err)
	}
	if got != path {
		t.Errorf("got %s, want %s", got, path)
	}

	// Verify the binary exists and is executable
	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat built binary: %v", err)
	}
	if info.Size() == 0 {
		t.Error("built binary is empty")
	}
	if info.Mode()&0o111 == 0 {
		t.Error("built binary is not executable")
	}
}

func TestEnsureBuilt_RealBuild_CrossCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow cross-compile test in short mode")
	}
	version := "test-cross-linux-" + t.Name()
	path := CachePath(version, "linux", "amd64")
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })

	got, err := EnsureBuilt(version, "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureBuilt linux/amd64: %v", err)
	}

	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == 0 {
		t.Error("cross-compiled binary is empty")
	}
}

func TestParseAgentVersionOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{"normal version", "weft-agent abc123def456", "abc123def456"},
		{"dev version", "weft-agent dev", "dev"},
		{"empty output", "", ""},
		{"no agent installed", " ", ""},
		{"wrong binary name", "other-agent v1.0", ""},
		{"only binary name", "weft-agent", ""},
		{"extra whitespace", "  weft-agent  abc123  ", "abc123"},
		{"with trailing newline", "weft-agent abc123\n", "abc123"},
		{"extra fields", "weft-agent abc123 extra stuff", "abc123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAgentVersionOutput(tt.output)
			if got != tt.want {
				t.Errorf("parseAgentVersionOutput(%q) = %q, want %q", tt.output, got, tt.want)
			}
		})
	}
}
