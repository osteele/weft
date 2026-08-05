package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSourceRootsRejectsNonSibling(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	other := filepath.Join(parent, "nested", "other")
	mustMkdir(t, project)
	mustMkdir(t, other)
	mustWrite(t, filepath.Join(project, ".weft.toml"), "[sync]\nsibling_roots = [\"../nested/other\"]\n")

	_, err := ResolveSourceRoots(project)
	if err == nil || !strings.Contains(err.Error(), "must share parent") {
		t.Fatalf("ResolveSourceRoots err = %v, want true-sibling validation error", err)
	}
}

func TestResolveSourceRootsRejectsBasenameCollision(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	mustMkdir(t, project)
	mustWrite(t, filepath.Join(project, ".weft.toml"), "[sync]\nsibling_roots = [\"../project\"]\n")

	_, err := ResolveSourceRoots(project)
	if err == nil || !strings.Contains(err.Error(), "basename collision") {
		t.Fatalf("ResolveSourceRoots err = %v, want basename collision", err)
	}
}

func TestResolveSourceRootsRejectsMissingSibling(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	mustMkdir(t, project)
	mustWrite(t, filepath.Join(project, ".weft.toml"), "[sync]\nsibling_roots = [\"../missing\"]\n")

	_, err := ResolveSourceRoots(project)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("ResolveSourceRoots err = %v, want missing-root error", err)
	}
}

func TestBuildSourceManifestAppliesExcludesToSiblingRoots(t *testing.T) {
	parent := t.TempDir()
	project := filepath.Join(parent, "project")
	sibling := filepath.Join(parent, "research-expkit")
	mustMkdir(t, project)
	mustMkdir(t, sibling)
	mustWrite(t, filepath.Join(project, ".weft.toml"), "[sync]\nsibling_roots = [\"../research-expkit\"]\n")
	mustWrite(t, filepath.Join(project, "main.py"), "print('ok')\n")
	mustWrite(t, filepath.Join(sibling, "pkg.py"), "value = 1\n")
	mustWrite(t, filepath.Join(sibling, ".venv", "ignored.py"), "ignored\n")
	mustWrite(t, filepath.Join(sibling, ".git", "config"), "ignored\n")

	manifest, tmpPaths, err := BuildSourceManifest(project)
	if err != nil {
		t.Fatalf("BuildSourceManifest: %v", err)
	}
	defer removeFiles(tmpPaths)
	if len(manifest.Roots) != 2 {
		t.Fatalf("roots = %d, want 2", len(manifest.Roots))
	}

	extract := t.TempDir()
	if err := ExtractTarball(tmpPaths[1], extract); err != nil {
		t.Fatalf("ExtractTarball sibling: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extract, "pkg.py")); err != nil {
		t.Fatalf("included file missing: %v", err)
	}
	for _, rel := range []string{".venv/ignored.py", ".git/config"} {
		if _, err := os.Stat(filepath.Join(extract, rel)); err == nil {
			t.Fatalf("excluded path %s was present in sibling snapshot", rel)
		}
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
