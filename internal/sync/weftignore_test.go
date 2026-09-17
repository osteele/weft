package sync

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// tarballEntryNames returns every entry name in the tarball at path.
func tarballEntryNames(t *testing.T, path string) []string {
	t.Helper()
	compressed, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	var names []string
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	return names
}

func TestParseWeftignorePatterns(t *testing.T) {
	tmpDir := t.TempDir()
	weftignore := `# Comment
corpus/
*.sqlite

!keep.sqlite
nested/path/thing
`
	if err := os.WriteFile(filepath.Join(tmpDir, ".weftignore"), []byte(weftignore), 0o644); err != nil {
		t.Fatal(err)
	}

	patterns := parseWeftignorePatterns(tmpDir)
	for _, want := range []string{"corpus", "*.sqlite"} {
		if !slices.Contains(patterns, want) {
			t.Errorf("missing expected pattern %q, got: %v", want, patterns)
		}
	}
	for _, bad := range []string{"!keep.sqlite", "keep.sqlite", "nested/path/thing"} {
		if slices.Contains(patterns, bad) {
			t.Errorf("should not contain %q, got: %v", bad, patterns)
		}
	}
}

func TestParseWeftignorePatternsNoFile(t *testing.T) {
	if patterns := parseWeftignorePatterns(t.TempDir()); len(patterns) != 0 {
		t.Errorf("expected empty patterns for dir without .weftignore, got: %v", patterns)
	}
}

func TestSourceExcludesIncludesWeftignore(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, ".weftignore"), []byte("corpus/\n*.sqlite\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	excludes := sourceExcludes(tmpDir)
	for _, want := range []string{"corpus", "*.sqlite"} {
		if !slices.Contains(excludes, want) {
			t.Errorf("sourceExcludes missing .weftignore pattern %q", want)
		}
	}
}

// TestCreateSourceTarballHonorsWeftignore is the wb148 regression: the cloud
// source path advised .weftignore as the way to narrow a source root while no
// code read the file, so the advice could not be acted on.
func TestCreateSourceTarballHonorsWeftignore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "corpus"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corpus", "big.bin"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".weftignore"), []byte("corpus/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, _, err := CreateSourceTarball(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	names := tarballEntryNames(t, path)
	if !slices.Contains(names, "main.py") {
		t.Errorf("main.py missing from source tarball: %v", names)
	}
	for _, name := range names {
		if name == "corpus" || name == "corpus/" || filepath.Dir(name) == "corpus" {
			t.Errorf(".weftignore'd path %q was included in the source tarball", name)
		}
	}
}

func TestGitignoreFiltersIncludeWeftignore(t *testing.T) {
	filters := gitignoreFilters(t.TempDir())
	if !slices.Contains(filters, ":- .weftignore") {
		t.Errorf("rsync filters missing .weftignore dir-merge rule: %v", filters)
	}
}
