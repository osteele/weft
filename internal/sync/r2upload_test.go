package sync

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type countingSourceStore struct {
	objects map[string][]byte
	puts    map[string]int
}

func newCountingSourceStore() *countingSourceStore {
	return &countingSourceStore{objects: make(map[string][]byte), puts: make(map[string]int)}
}

func (s *countingSourceStore) ObjectExists(_ context.Context, key string) (bool, error) {
	_, ok := s.objects[key]
	return ok, nil
}

func (s *countingSourceStore) PutObject(_ context.Context, key string, body io.Reader, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.objects[key] = data
	s.puts[key]++
	return nil
}

func writeSparseTestFile(t *testing.T, path string, size int64) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sparse file %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close sparse file %s: %v", path, err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("truncate sparse file %s: %v", path, err)
	}
}

func testFileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func assertTarPathMissing(t *testing.T, tarPath, relPath string) {
	t.Helper()
	extractDir := t.TempDir()
	if err := ExtractTarball(tarPath, extractDir); err != nil {
		t.Fatalf("ExtractTarball: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extractDir, filepath.FromSlash(relPath))); !os.IsNotExist(err) {
		t.Fatalf("tar path %s stat error = %v, want absent", relPath, err)
	}
}

func TestBuildCloudSourceManifestDivertsLargeFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	largePath := filepath.Join(dir, "data", "training.pkl")
	if err := os.MkdirAll(filepath.Dir(largePath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSparseTestFile(t, largePath, LargeSourceBlobThresholdBytes)
	wantHash := testFileSHA256(t, largePath)

	manifest, tarPaths, err := BuildCloudSourceManifestForInputsAndCommands(dir, nil, nil)
	if err != nil {
		t.Fatalf("BuildCloudSourceManifestForInputsAndCommands: %v", err)
	}
	defer removeFiles(tarPaths)
	if len(manifest.Roots) != 1 || len(manifest.Roots[0].Blobs) != 1 {
		t.Fatalf("manifest roots/blobs = %+v, want one root with one blob", manifest.Roots)
	}
	blob := manifest.Roots[0].Blobs[0]
	if blob.RelPath != "data/training.pkl" || blob.SHA256 != wantHash || blob.R2Key != "assets/"+wantHash {
		t.Fatalf("blob = %+v, want path data/training.pkl and hash/key %s", blob, wantHash)
	}
	assertTarPathMissing(t, tarPaths[0], blob.RelPath)
}

func TestBuildCloudSourceManifestDivertsLocalOverlay(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("data/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	largePath := filepath.Join(dir, "data", "overlay.bin")
	if err := os.MkdirAll(filepath.Dir(largePath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSparseTestFile(t, largePath, LargeSourceBlobThresholdBytes)

	manifest, tarPaths, err := BuildCloudSourceManifestForInputsAndCommands(dir, []string{"local:data/overlay.bin"}, nil)
	if err != nil {
		t.Fatalf("BuildCloudSourceManifestForInputsAndCommands: %v", err)
	}
	defer removeFiles(tarPaths)
	if len(manifest.Roots) != 1 || len(manifest.Roots[0].Blobs) != 1 {
		t.Fatalf("manifest roots/blobs = %+v, want overlaid file diverted", manifest.Roots)
	}
	if got := manifest.Roots[0].Blobs[0].RelPath; got != "data/overlay.bin" {
		t.Fatalf("blob rel_path = %q, want data/overlay.bin", got)
	}
	assertTarPathMissing(t, tarPaths[0], "data/overlay.bin")
}

func TestUploadCloudSourceReusesLargeBlob(t *testing.T) {
	dir := t.TempDir()
	largePath := filepath.Join(dir, "large.bin")
	writeSparseTestFile(t, largePath, LargeSourceBlobThresholdBytes)
	store := newCountingSourceStore()

	first, err := uploadSourceRootsToR2(context.Background(), store, dir, nil, nil, nil, true)
	if err != nil {
		t.Fatalf("first uploadSourceRootsToR2: %v", err)
	}
	second, err := uploadSourceRootsToR2(context.Background(), store, dir, nil, nil, nil, true)
	if err != nil {
		t.Fatalf("second uploadSourceRootsToR2: %v", err)
	}
	blobKey := first.Manifest.Roots[0].Blobs[0].R2Key
	if second.Manifest.Hash != first.Manifest.Hash {
		t.Fatalf("manifest hash changed: %s -> %s", first.Manifest.Hash, second.Manifest.Hash)
	}
	if got := store.puts[blobKey]; got != 1 {
		t.Fatalf("blob put count = %d, want 1 for %s", got, blobKey)
	}
}

func TestCloudSourceManifestIdentityIncludesDivertedFile(t *testing.T) {
	dir := t.TempDir()
	largePath := filepath.Join(dir, "large.bin")
	writeSparseTestFile(t, largePath, LargeSourceBlobThresholdBytes)
	first, firstTarPaths, err := BuildCloudSourceManifestForInputsAndCommands(dir, nil, nil)
	if err != nil {
		t.Fatalf("first manifest: %v", err)
	}
	defer removeFiles(firstTarPaths)
	f, err := os.OpenFile(largePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{1}); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	second, secondTarPaths, err := BuildCloudSourceManifestForInputsAndCommands(dir, nil, nil)
	if err != nil {
		t.Fatalf("second manifest: %v", err)
	}
	defer removeFiles(secondTarPaths)
	if first.Roots[0].Hash != second.Roots[0].Hash {
		t.Fatalf("residual tarball hashes differ: %s vs %s", first.Roots[0].Hash, second.Roots[0].Hash)
	}
	if first.Hash == second.Hash {
		t.Fatalf("manifest hash = %s for two different diverted files", first.Hash)
	}
}

func TestBuildCloudSourceManifestKeepsFileBelowThreshold(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "almost-large.bin")
	writeSparseTestFile(t, filePath, LargeSourceBlobThresholdBytes-1)
	manifest, tarPaths, err := BuildCloudSourceManifestForInputsAndCommands(dir, nil, nil)
	if err != nil {
		t.Fatalf("BuildCloudSourceManifestForInputsAndCommands: %v", err)
	}
	defer removeFiles(tarPaths)
	if got := len(manifest.Roots[0].Blobs); got != 0 {
		t.Fatalf("blob count = %d, want 0 below threshold", got)
	}
	extractDir := t.TempDir()
	if err := ExtractTarball(tarPaths[0], extractDir); err != nil {
		t.Fatalf("ExtractTarball: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extractDir, "almost-large.bin")); err != nil {
		t.Fatalf("below-threshold file missing from tarball: %v", err)
	}
}

func TestCloudSourceResidualOverflowAdvice(t *testing.T) {
	err := SourceSizeLimitError(MaxSourceTarballBytes+1, []string{"local:data"})
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("error = %v, want ErrSourceTooLarge", err)
	}
	if !strings.Contains(err.Error(), "diverted automatically") {
		t.Fatalf("error does not explain residual size after automatic diversion: %v", err)
	}
	if strings.Contains(err.Error(), "weft data publish") {
		t.Fatalf("error still recommends manual publishing for automatically diverted files: %v", err)
	}
}

func TestStageSourceDirWithLocalInputs_OverridesGitignore(t *testing.T) {
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, ".gitignore"), []byte("data/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write main.py: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(localDir, "data", "conllu"), 0o755); err != nil {
		t.Fatalf("mkdir data/conllu: %v", err)
	}
	explicitFile := filepath.Join(localDir, "data", "conllu", "train.conllu")
	if err := os.WriteFile(explicitFile, []byte("1\ttest\n"), 0o644); err != nil {
		t.Fatalf("write explicit input: %v", err)
	}

	baseTar, _, err := CreateSourceTarball(localDir)
	if err != nil {
		t.Fatalf("CreateSourceTarball: %v", err)
	}
	defer os.Remove(baseTar)
	baseExtract := t.TempDir()
	if err := ExtractTarball(baseTar, baseExtract); err != nil {
		t.Fatalf("ExtractTarball base: %v", err)
	}
	if _, err := os.Stat(filepath.Join(baseExtract, "data", "conllu", "train.conllu")); err == nil {
		t.Fatalf("expected gitignored file to be absent from base tarball")
	}

	stageDir, _, cleanup, err := stageSourceDirWithLocalInputs(localDir, []string{"local:data/conllu/"})
	if err != nil {
		t.Fatalf("stageSourceDirWithLocalInputs: %v", err)
	}
	defer cleanup()
	if stageDir == "" || stageDir == localDir {
		t.Fatalf("expected a staged directory, got %q", stageDir)
	}

	stagedTar, _, err := createSourceTarballWithOverlays(stageDir, nil, nil)
	if err != nil {
		t.Fatalf("createSourceTarball(stage,nil): %v", err)
	}
	defer os.Remove(stagedTar)
	stagedExtract := t.TempDir()
	if err := ExtractTarball(stagedTar, stagedExtract); err != nil {
		t.Fatalf("ExtractTarball staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stagedExtract, "data", "conllu", "train.conllu")); err != nil {
		t.Fatalf("expected explicit local input to be present: %v", err)
	}
}

func TestLocalInputOverlays_ValidatePaths(t *testing.T) {
	localDir := t.TempDir()

	t.Run("missing local input", func(t *testing.T) {
		_, err := LocalInputOverlays(localDir, []string{"local:data/conllu/"}, RequireLocalInput)
		if err == nil {
			t.Fatal("expected error for missing local input")
		}
	})

	t.Run("escaping local input", func(t *testing.T) {
		parent := filepath.Dir(localDir)
		outside := filepath.Join(parent, "outside")
		if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
			t.Fatalf("write outside file: %v", err)
		}
		defer os.Remove(outside)
		_, err := LocalInputOverlays(localDir, []string{"local:../outside"}, RequireLocalInput)
		if err == nil {
			t.Fatal("expected error for escaping local input")
		}
	})
}

func TestOverlayTarball_EndToEnd(t *testing.T) {
	// Set up a project directory mimicking the real scenario:
	// .gitignore excludes data/, but local:data/conllu/ should be included.
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, ".gitignore"), []byte("data/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "main.py"), []byte("print('hello')\n"), 0o644); err != nil {
		t.Fatalf("write main.py: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(localDir, "data", "conllu"), 0o755); err != nil {
		t.Fatalf("mkdir data/conllu: %v", err)
	}
	// Create multiple files to match the real scenario
	files := map[string]string{
		"data/conllu/train.conllu": "1\ttrain\tdata\n",
		"data/conllu/dev.conllu":   "1\tdev\tdata\n",
		"data/conllu/test.conllu":  "1\ttest\tdata\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(localDir, rel), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// Stage with local: input overlay
	inputs := []string{"hf:bert-base-cased", "local:data/conllu/"}
	stagedDir, _, cleanup, err := stageSourceDirWithLocalInputs(localDir, inputs)
	if err != nil {
		t.Fatalf("stageSourceDirWithLocalInputs: %v", err)
	}
	defer cleanup()

	if stagedDir == "" {
		t.Fatal("expected a staged directory, got empty string")
	}

	// Create tarball from staged dir (no excludes)
	tmpPath, hashWithOverlay, err := createSourceTarballWithOverlays(stagedDir, nil, nil)
	if err != nil {
		t.Fatalf("createSourceTarball(staged): %v", err)
	}
	defer os.Remove(tmpPath)

	// Create base tarball (with excludes) for hash comparison
	baseTmpPath, hashWithout, err := CreateSourceTarball(localDir)
	if err != nil {
		t.Fatalf("CreateSourceTarball(base): %v", err)
	}
	defer os.Remove(baseTmpPath)

	if hashWithOverlay == hashWithout {
		t.Fatal("overlay tarball hash should differ from base tarball hash")
	}

	// Extract the overlay tarball and verify contents
	extractDir := t.TempDir()
	if err := ExtractTarball(tmpPath, extractDir); err != nil {
		t.Fatalf("ExtractTarball: %v", err)
	}

	// Verify main.py exists
	if _, err := os.Stat(filepath.Join(extractDir, "main.py")); err != nil {
		t.Errorf("expected main.py in extracted tarball: %v", err)
	}

	// Verify all overlay files exist with correct content
	for rel, wantContent := range files {
		got, err := os.ReadFile(filepath.Join(extractDir, rel))
		if err != nil {
			t.Errorf("expected %s in extracted tarball: %v", rel, err)
			continue
		}
		if string(got) != wantContent {
			t.Errorf("content mismatch for %s: got %q, want %q", rel, got, wantContent)
		}
	}
}

func TestBuildSourceSnapshot_UsesDefaultExcludesAndLocalInputOverlays(t *testing.T) {
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, ".gitignore"), []byte("data/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write main.py: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(localDir, "data", "conllu"), 0o755); err != nil {
		t.Fatalf("mkdir data/conllu: %v", err)
	}
	overlayFile := filepath.Join(localDir, "data", "conllu", "train.conllu")
	if err := os.WriteFile(overlayFile, []byte("1\ttest\n"), 0o644); err != nil {
		t.Fatalf("write overlay file: %v", err)
	}

	noOverlay, err := BuildSourceSnapshot(localDir, nil)
	if err != nil {
		t.Fatalf("BuildSourceSnapshot(no overlays): %v", err)
	}
	defer noOverlay.Cleanup()
	if _, err := os.Stat(filepath.Join(noOverlay.Dir, "main.py")); err != nil {
		t.Fatalf("expected main.py in snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(noOverlay.Dir, "data", "conllu", "train.conllu")); err == nil {
		t.Fatalf("expected gitignored file to be absent without explicit local input")
	}

	withOverlay, err := BuildSourceSnapshot(localDir, []string{"local:data/conllu/"})
	if err != nil {
		t.Fatalf("BuildSourceSnapshot(with overlays): %v", err)
	}
	defer withOverlay.Cleanup()
	if _, err := os.Stat(filepath.Join(withOverlay.Dir, "data", "conllu", "train.conllu")); err != nil {
		t.Fatalf("expected explicit local input to be present: %v", err)
	}
}

func TestBuildSourceSnapshot_OverlaysLocalFileInput(t *testing.T) {
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, ".gitignore"), []byte("data/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("write main.py: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(localDir, "data"), 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "data", "calibration.db"), []byte("db"), 0o644); err != nil {
		t.Fatalf("write calibration db: %v", err)
	}

	withOverlay, err := BuildSourceSnapshot(localDir, []string{"local:data/calibration.db"})
	if err != nil {
		t.Fatalf("BuildSourceSnapshot(with file overlay): %v", err)
	}
	defer withOverlay.Cleanup()
	if _, err := os.Stat(filepath.Join(withOverlay.Dir, "data", "calibration.db")); err != nil {
		t.Fatalf("expected explicit local file input to be present: %v", err)
	}
}

// Regression (wb18/wj2812): when declared local: inputs push the staged
// source over the size limit, the error must name the inputs and point at
// the asset store — .gitignore/.weftignore cannot exclude overlaid inputs.
func TestUploadSource_OverlayOverflowAdvice(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "outputs", "representations")
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}
	// .gitignore'd (so the base tree excludes it) but declared as an input.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("outputs/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		writeSparseTestFile(t, filepath.Join(big, fmt.Sprintf("part%d.pkl", i)), MaxSourceTarballBytes/4+1)
	}

	stagedDir, overlayInputs, cleanup, err := stageSourceDirWithLocalInputs(dir, []string{"local:outputs/representations"})
	if err != nil {
		t.Fatalf("stageSourceDirWithLocalInputs: %v", err)
	}
	defer cleanup()
	if stagedDir == "" {
		t.Fatal("expected staged overlay dir")
	}

	_, _, err = createSourceTarballWithOverlays(stagedDir, nil, overlayInputs)
	if err == nil {
		t.Fatal("expected size-limit error")
	}
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Errorf("error not ErrSourceTooLarge: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "local:outputs/representations") {
		t.Errorf("error does not name the overlaid input:\n%s", msg)
	}
	if !strings.Contains(msg, "weft data publish") || !strings.Contains(msg, "--input asset:") {
		t.Errorf("error does not point at the asset store:\n%s", msg)
	}
	if strings.Contains(msg, "Add large directories to .gitignore") {
		t.Errorf("overlay overflow must not advise .gitignore:\n%s", msg)
	}
}

// A plain (non-overlay) overflow keeps the exclude advice.
func TestCreateSourceTarball_PlainOverflowAdvice(t *testing.T) {
	dir := t.TempDir()
	for i := range 5 {
		writeSparseTestFile(t, filepath.Join(dir, fmt.Sprintf("big%d.bin", i)), MaxSourceTarballBytes/4+1)
	}
	_, _, err := CreateSourceTarball(dir)
	if err == nil {
		t.Fatal("expected size-limit error")
	}
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Errorf("error not ErrSourceTooLarge: %v", err)
	}
	if !strings.Contains(err.Error(), ".weftignore") {
		t.Errorf("plain overflow should advise excludes:\n%v", err)
	}
}
