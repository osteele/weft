package agentdeploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestEnsureBuiltWithProgress_CacheHitReportsLocalCache(t *testing.T) {
	version := "test-progress-cache-hit-" + t.Name()
	path := CachePath(version, "linux", "amd64")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fake-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })

	var phases []string
	got, err := EnsureBuiltWithProgress(version, "linux", "amd64", nil, func(phase string) {
		phases = append(phases, phase)
	})
	if err != nil {
		t.Fatalf("EnsureBuiltWithProgress: %v", err)
	}
	if got != path {
		t.Errorf("got %s, want %s", got, path)
	}
	want := []string{"checking local agent cache", "using local agent cache"}
	if strings.Join(phases, "|") != strings.Join(want, "|") {
		t.Fatalf("phases = %v, want %v", phases, want)
	}
}

func TestEnsureBuilt_CacheMiss_InvokesExtract(t *testing.T) {
	var captured struct {
		version, goos, goarch, outputPath string
	}

	cleanup := SetExtractFunc(func(version, goos, goarch, outputPath string) error {
		captured.version = version
		captured.goos = goos
		captured.goarch = goarch
		captured.outputPath = outputPath
		return os.WriteFile(outputPath, []byte("fake-agent"), 0o755)
	})
	defer cleanup()

	version := "test-mock-extract-" + t.Name()
	path := CachePath(version, "linux", "amd64")
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })

	got, err := EnsureBuilt(version, "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureBuilt: %v", err)
	}
	if got != path {
		t.Errorf("got %s, want %s", got, path)
	}

	if captured.version != version {
		t.Errorf("version = %q, want %q", captured.version, version)
	}
	if captured.goos != "linux" {
		t.Errorf("GOOS = %q, want %q", captured.goos, "linux")
	}
	if captured.goarch != "amd64" {
		t.Errorf("GOARCH = %q, want %q", captured.goarch, "amd64")
	}
	if captured.outputPath != path {
		t.Errorf("output path = %q, want %q", captured.outputPath, path)
	}
}

func TestEnsureBuilt_CacheHit_SkipsExtract(t *testing.T) {
	version := "test-skip-extract-" + t.Name()
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

	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "sentinel-value" {
		t.Error("cache hit should return existing file without extracting")
	}
}

func TestEnsureBuilt_ConcurrentCalls_SingleExtraction(t *testing.T) {
	var extractCalls int32
	cleanup := SetExtractFunc(func(version, goos, goarch, outputPath string) error {
		atomic.AddInt32(&extractCalls, 1)
		// Keep extraction slow enough to force concurrent overlap.
		time.Sleep(120 * time.Millisecond)
		return os.WriteFile(outputPath, []byte("fake-agent"), 0o755)
	})
	defer cleanup()

	version := "test-concurrent-build-" + t.Name()
	path := CachePath(version, "linux", "amd64")
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })

	const workers = 6
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	results := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := EnsureBuilt(version, "linux", "amd64")
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}
	wg.Wait()
	close(errs)
	close(results)

	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureBuilt concurrent call failed: %v", err)
		}
	}
	for got := range results {
		if got != path {
			t.Fatalf("result path = %q, want %q", got, path)
		}
	}
	if calls := atomic.LoadInt32(&extractCalls); calls != 1 {
		t.Fatalf("extract called %d times, want 1", calls)
	}
}

func TestEnsureBuilt_ExtractFromFilesystem(t *testing.T) {
	originalRepoRoot := buildRepoRootFunc
	t.Cleanup(func() {
		buildRepoRootFunc = originalRepoRoot
	})

	root := t.TempDir()
	binDir := filepath.Join(root, "internal", "agentdeploy", "binaries")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	version := "test-filesystem-extract-" + t.Name()
	if err := os.WriteFile(filepath.Join(binDir, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "weft-agent-linux-amd64"), []byte("fake-agent-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	buildRepoRootFunc = func() (string, error) {
		return root, nil
	}

	path := CachePath(version, "linux", "amd64")
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(filepath.Dir(path))) })

	got, err := EnsureBuilt(version, "linux", "amd64")
	if err != nil {
		t.Fatalf("EnsureBuilt: %v", err)
	}

	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat extracted binary: %v", err)
	}
	if info.Size() == 0 {
		t.Error("extracted binary is empty")
	}
	if info.Mode()&0o111 == 0 {
		t.Error("extracted binary is not executable")
	}
}

func TestLocalAgentVersion_HashFormatAndStability(t *testing.T) {
	version1, err := LocalAgentVersion()
	if err != nil {
		t.Fatalf("LocalAgentVersion: %v", err)
	}
	version2, err := LocalAgentVersion()
	if err != nil {
		t.Fatalf("LocalAgentVersion second call: %v", err)
	}
	if version1 != version2 {
		t.Fatalf("LocalAgentVersion changed between calls: %q != %q", version1, version2)
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(version1) {
		t.Fatalf("LocalAgentVersion = %q, want 12 lowercase hex chars", version1)
	}
}

func TestHashVersionFromFiles_DeterministicOrdering(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a.txt")
	b := filepath.Join(root, "b.txt")
	if err := os.WriteFile(a, []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1, err := hashVersionFromFiles(root, []string{a, b})
	if err != nil {
		t.Fatalf("hashVersionFromFiles: %v", err)
	}
	h2, err := hashVersionFromFiles(root, []string{b, a})
	if err != nil {
		t.Fatalf("hashVersionFromFiles reversed: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash should be deterministic across file order, got %q and %q", h1, h2)
	}
}

func TestHashVersionFromFiles_ChangesWithContent(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "agent.go")
	if err := os.WriteFile(file, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := hashVersionFromFiles(root, []string{file})
	if err != nil {
		t.Fatalf("hashVersionFromFiles initial: %v", err)
	}
	if err := os.WriteFile(file, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	h2, err := hashVersionFromFiles(root, []string{file})
	if err != nil {
		t.Fatalf("hashVersionFromFiles updated: %v", err)
	}
	if h1 == h2 {
		t.Fatalf("hash should change when file content changes: %q", h1)
	}
}

func TestHashVersionFromFiles_ChangesWithPath(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", "same.txt")
	b := filepath.Join(root, "b", "same.txt")
	if err := os.MkdirAll(filepath.Dir(a), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(b), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a, []byte("identical"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("identical"), 0o644); err != nil {
		t.Fatal(err)
	}

	h1, err := hashVersionFromFiles(root, []string{a})
	if err != nil {
		t.Fatalf("hashVersionFromFiles for a: %v", err)
	}
	h2, err := hashVersionFromFiles(root, []string{b})
	if err != nil {
		t.Fatalf("hashVersionFromFiles for b: %v", err)
	}
	if h1 == h2 {
		t.Fatalf("hash should include relative path; got equal hash %q", h1)
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
