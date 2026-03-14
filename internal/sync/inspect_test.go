package sync

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestInspectSnapshotHonorsExcludesAndSummarizesSizes(t *testing.T) {
	tmpDir := t.TempDir()

	writeTestFile(t, filepath.Join(tmpDir, "keep.txt"), 10)
	writeTestFile(t, filepath.Join(tmpDir, "src", "main.go"), 20)
	writeTestFile(t, filepath.Join(tmpDir, "src", "big.bin"), 200)
	writeTestFile(t, filepath.Join(tmpDir, "data", "raw.bin"), 500)
	writeTestFile(t, filepath.Join(tmpDir, "results", "metrics.json"), 300)
	writeTestFile(t, filepath.Join(tmpDir, ".git", "HEAD"), 100)
	if err := os.WriteFile(filepath.Join(tmpDir, ".weft.toml"), []byte("[sync]\nexclude_dirs = [\"data\"]\n[outputs]\ndirs = [\"results/\"]\n"), 0o644); err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	got, err := InspectSnapshot(tmpDir, 10, 10)
	if err != nil {
		t.Fatalf("InspectSnapshot: %v", err)
	}

	if got.TotalBytes != 230 {
		t.Fatalf("TotalBytes = %d, want 230", got.TotalBytes)
	}
	if got.FileCount != 3 {
		t.Fatalf("FileCount = %d, want 3", got.FileCount)
	}
	if got.DirectoryCount != 1 {
		t.Fatalf("DirectoryCount = %d, want 1", got.DirectoryCount)
	}
	if got.OverLimit {
		t.Fatal("OverLimit = true, want false")
	}
	if got.CompressedBytes == nil || *got.CompressedBytes == 0 {
		t.Fatal("CompressedBytes = nil/0, want non-zero")
	}
	if !slices.Contains(got.Excludes, "data") {
		t.Fatalf("Excludes missing project exclude: %v", got.Excludes)
	}
	if !slices.Contains(got.Excludes, "results") {
		t.Fatalf("Excludes missing output dir: %v", got.Excludes)
	}

	if len(got.LargestFiles) < 2 {
		t.Fatalf("LargestFiles len = %d, want at least 2", len(got.LargestFiles))
	}
	if got.LargestFiles[0].Path != filepath.Join("src", "big.bin") || got.LargestFiles[0].Bytes != 200 {
		t.Fatalf("LargestFiles[0] = %+v", got.LargestFiles[0])
	}
	if got.LargestFiles[0].ApproxCompressedBytes <= 0 {
		t.Fatalf("LargestFiles[0].ApproxCompressedBytes = %d, want > 0", got.LargestFiles[0].ApproxCompressedBytes)
	}
	if len(got.LargestTopLevel) != 1 || got.LargestTopLevel[0].Path != "src" || got.LargestTopLevel[0].Bytes != 220 {
		t.Fatalf("LargestTopLevel = %+v, want src=220", got.LargestTopLevel)
	}
	if got.LargestTopLevel[0].ApproxCompressedBytes <= 0 {
		t.Fatalf("LargestTopLevel[0].ApproxCompressedBytes = %d, want > 0", got.LargestTopLevel[0].ApproxCompressedBytes)
	}
}

func TestInspectSnapshotTrimsRankedLists(t *testing.T) {
	tmpDir := t.TempDir()
	writeTestFile(t, filepath.Join(tmpDir, "a.txt"), 10)
	writeTestFile(t, filepath.Join(tmpDir, "b.txt"), 20)
	writeTestFile(t, filepath.Join(tmpDir, "dir1", "one.bin"), 30)
	writeTestFile(t, filepath.Join(tmpDir, "dir2", "two.bin"), 40)

	got, err := InspectSnapshot(tmpDir, 2, 1)
	if err != nil {
		t.Fatalf("InspectSnapshot: %v", err)
	}

	if len(got.LargestFiles) != 2 {
		t.Fatalf("LargestFiles len = %d, want 2", len(got.LargestFiles))
	}
	if got.LargestFiles[0].ApproxCompressedBytes < got.LargestFiles[1].ApproxCompressedBytes {
		t.Fatalf("LargestFiles not sorted by approx compressed bytes: %+v", got.LargestFiles)
	}
	if len(got.LargestTopLevel) != 1 {
		t.Fatalf("LargestTopLevel len = %d, want 1", len(got.LargestTopLevel))
	}
	if got.LargestTopLevel[0].Path != "dir1" && got.LargestTopLevel[0].Path != "dir2" {
		t.Fatalf("LargestTopLevel[0] = %+v, want dir1 or dir2", got.LargestTopLevel[0])
	}
	if got.LargestFiles[0].ApproxCompressedBytes <= 0 {
		t.Fatalf("LargestFiles[0].ApproxCompressedBytes = %d, want > 0", got.LargestFiles[0].ApproxCompressedBytes)
	}
}

func TestInspectSnapshotRanksByApproxCompressedBytes(t *testing.T) {
	tmpDir := t.TempDir()

	writeTestFileWithContent(t, filepath.Join(tmpDir, "packed.bin"), bytes.Repeat([]byte("A"), 4096))
	randomData := make([]byte, 2048)
	if _, err := rand.Read(randomData); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	writeTestFileWithContent(t, filepath.Join(tmpDir, "random.bin"), randomData)

	got, err := InspectSnapshot(tmpDir, 2, 1)
	if err != nil {
		t.Fatalf("InspectSnapshot: %v", err)
	}

	if len(got.LargestFiles) != 2 {
		t.Fatalf("LargestFiles len = %d, want 2", len(got.LargestFiles))
	}
	if got.LargestFiles[0].Path != "random.bin" {
		t.Fatalf("LargestFiles[0] = %+v, want random.bin first", got.LargestFiles[0])
	}
	if got.LargestFiles[0].ApproxCompressedBytes <= got.LargestFiles[1].ApproxCompressedBytes {
		t.Fatalf("approx compressed bytes not sorted descending: %+v", got.LargestFiles)
	}
}

func writeTestFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	content := make([]byte, size)
	for i := range content {
		content[i] = byte((i*31 + len(path)) % 251)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func writeTestFileWithContent(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
