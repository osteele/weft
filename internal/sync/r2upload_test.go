package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

	stagedTar, _, err := createSourceTarball(stageDir, nil)
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
	tmpPath, hashWithOverlay, err := createSourceTarball(stagedDir, nil)
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
	data := make([]byte, MaxSourceTarballBytes/4+1)
	for i := range 5 {
		if err := os.WriteFile(filepath.Join(big, fmt.Sprintf("part%d.pkl", i)), data, 0o644); err != nil {
			t.Fatal(err)
		}
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
	data := make([]byte, MaxSourceTarballBytes/4+1)
	for i := range 5 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("big%d.bin", i)), data, 0o644); err != nil {
			t.Fatal(err)
		}
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
