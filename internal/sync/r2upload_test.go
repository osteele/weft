package sync

import (
	"os"
	"path/filepath"
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

	stageDir, cleanup, err := stageSourceDirWithLocalInputs(localDir, []string{"local:data/conllu/"})
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
		_, err := localInputOverlays(localDir, []string{"local:data/conllu/"})
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
		_, err := localInputOverlays(localDir, []string{"local:../outside"})
		if err == nil {
			t.Fatal("expected error for escaping local input")
		}
	})
}
