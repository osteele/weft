package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSyncInspectPrintsSummary(t *testing.T) {
	tmpDir := t.TempDir()
	writeSyncInspectFile(t, filepath.Join(tmpDir, "src", "big.bin"), 200)
	writeSyncInspectFile(t, filepath.Join(tmpDir, "src", "small.txt"), 20)
	writeSyncInspectFile(t, filepath.Join(tmpDir, "data", "ignored.bin"), 500)
	if err := os.WriteFile(filepath.Join(tmpDir, ".weft.toml"), []byte("[sync]\nexclude_dirs = [\"data\"]\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	var buf bytes.Buffer
	syncInspectCmd.SetOut(&buf)
	syncInspectCmd.SetErr(&buf)
	syncInspectTopFiles = 5
	syncInspectTopDirs = 5
	syncInspectJSON = false
	syncInspectShowExcludes = false

	if err := runSyncInspect(syncInspectCmd, []string{tmpDir}); err != nil {
		t.Fatalf("runSyncInspect: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"Source snapshot:",
		"Included files: 2",
		"Included size:",
		"Compressed tarball:",
		"Largest included files:",
		"~",
		filepath.Join("src", "big.bin"),
		"Largest included top-level directories:",
		"src",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunSyncInspectJSONIncludesExcludes(t *testing.T) {
	tmpDir := t.TempDir()
	writeSyncInspectFile(t, filepath.Join(tmpDir, "keep.txt"), 10)
	if err := os.WriteFile(filepath.Join(tmpDir, ".weft.toml"), []byte("[sync]\nexclude_dirs = [\"data\"]\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	var buf bytes.Buffer
	syncInspectCmd.SetOut(&buf)
	syncInspectCmd.SetErr(&buf)
	syncInspectTopFiles = 1
	syncInspectTopDirs = 1
	syncInspectJSON = true
	syncInspectShowExcludes = false

	if err := runSyncInspect(syncInspectCmd, []string{tmpDir}); err != nil {
		t.Fatalf("runSyncInspect: %v", err)
	}

	var payload struct {
		LocalDir     string   `json:"local_dir"`
		Excludes     []string `json:"excludes"`
		LargestFiles []struct {
			Path                  string `json:"path"`
			ApproxCompressedBytes int64  `json:"approx_compressed_bytes"`
		} `json:"largest_files"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, buf.String())
	}
	if payload.LocalDir != tmpDir {
		t.Fatalf("LocalDir = %q, want %q", payload.LocalDir, tmpDir)
	}
	if !containsString(payload.Excludes, "data") {
		t.Fatalf("Excludes missing data: %v", payload.Excludes)
	}
	if len(payload.LargestFiles) != 1 || payload.LargestFiles[0].Path != "keep.txt" || payload.LargestFiles[0].ApproxCompressedBytes <= 0 {
		t.Fatalf("LargestFiles = %+v, want keep.txt with approx compressed bytes", payload.LargestFiles)
	}
}

func writeSyncInspectFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
