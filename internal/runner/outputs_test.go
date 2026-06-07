package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverOutputs(t *testing.T) {
	t.Run("empty workdir", func(t *testing.T) {
		files, err := DiscoverOutputs("", []string{"output/"})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Errorf("expected no files, got %d", len(files))
		}
	})

	t.Run("empty dirs", func(t *testing.T) {
		files, err := DiscoverOutputs("/tmp", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Errorf("expected no files, got %d", len(files))
		}
	})

	t.Run("no output dir exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		files, err := DiscoverOutputs(tmpDir, []string{"output/", "outputs/"})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Errorf("expected no files, got %d", len(files))
		}
	})

	t.Run("discovers files in output dir", func(t *testing.T) {
		tmpDir := t.TempDir()
		outputDir := filepath.Join(tmpDir, "output")
		os.MkdirAll(outputDir, 0755)
		os.WriteFile(filepath.Join(outputDir, "results.json"), []byte(`{"accuracy": 0.95}`), 0644)
		os.WriteFile(filepath.Join(outputDir, "model.bin"), []byte("model data"), 0644)

		files, err := DiscoverOutputs(tmpDir, []string{"output/"})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 2 {
			t.Fatalf("expected 2 files, got %d", len(files))
		}

		// Check that paths are relative to workDir
		paths := map[string]bool{}
		for _, f := range files {
			paths[f.RelPath] = true
			if f.SizeBytes <= 0 {
				t.Errorf("expected positive size for %s, got %d", f.RelPath, f.SizeBytes)
			}
		}
		if !paths["output/results.json"] {
			t.Error("expected output/results.json")
		}
		if !paths["output/model.bin"] {
			t.Error("expected output/model.bin")
		}
	})

	t.Run("discovers files in nested subdirs", func(t *testing.T) {
		tmpDir := t.TempDir()
		subDir := filepath.Join(tmpDir, "output", "checkpoints")
		os.MkdirAll(subDir, 0755)
		os.WriteFile(filepath.Join(subDir, "epoch1.pt"), []byte("checkpoint"), 0644)

		files, err := DiscoverOutputs(tmpDir, []string{"output/"})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 {
			t.Fatalf("expected 1 file, got %d", len(files))
		}
		if files[0].RelPath != "output/checkpoints/epoch1.pt" {
			t.Errorf("unexpected path: %s", files[0].RelPath)
		}
	})

	t.Run("multiple output dirs", func(t *testing.T) {
		tmpDir := t.TempDir()
		os.MkdirAll(filepath.Join(tmpDir, "output"), 0755)
		os.MkdirAll(filepath.Join(tmpDir, "outputs"), 0755)
		os.WriteFile(filepath.Join(tmpDir, "output", "a.txt"), []byte("a"), 0644)
		os.WriteFile(filepath.Join(tmpDir, "outputs", "b.txt"), []byte("b"), 0644)

		files, err := DiscoverOutputs(tmpDir, []string{"output/", "outputs/"})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 2 {
			t.Fatalf("expected 2 files, got %d", len(files))
		}
	})

	t.Run("expands bare tilde", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		outputDir := filepath.Join(homeDir, "output")
		if err := os.MkdirAll(outputDir, 0o755); err != nil {
			t.Fatalf("mkdir output: %v", err)
		}
		if err := os.WriteFile(filepath.Join(outputDir, "results.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatalf("write results: %v", err)
		}

		files, err := DiscoverOutputs("~", []string{"output/"})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 || files[0].RelPath != "output/results.json" {
			t.Fatalf("unexpected files: %+v", files)
		}
	})
}

func TestTotalSizeMB(t *testing.T) {
	files := []OutputFile{
		{RelPath: "a", SizeBytes: 1024 * 1024},      // 1 MB
		{RelPath: "b", SizeBytes: 50 * 1024 * 1024}, // 50 MB
	}
	if got := TotalSizeMB(files); got != 51 {
		t.Errorf("expected 51 MB, got %d", got)
	}

	if got := TotalSizeMB(nil); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestDiscoverOutputRefs(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "output", "bayes_course"), 0o755); err != nil {
		t.Fatalf("mkdir output dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "output", "bayes_course", "result.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "metrics.json"), []byte(`{"loss": 1}`), 0o644); err != nil {
		t.Fatalf("write metrics: %v", err)
	}

	files, err := DiscoverOutputRefs(tmpDir, []string{
		"output/bayes_course/",
		"local:output/bayes_course/",
		"metrics.json",
		"hf:org/model",
		"missing/",
	})
	if err != nil {
		t.Fatal(err)
	}

	paths := map[string]bool{}
	for _, f := range files {
		paths[f.RelPath] = true
	}
	if !paths["output/bayes_course/result.json"] {
		t.Fatalf("expected declared output directory file, got %+v", files)
	}
	if !paths["metrics.json"] {
		t.Fatalf("expected declared output file, got %+v", files)
	}
	if paths["hf:org/model"] {
		t.Fatalf("data-location token was treated as a local file: %+v", files)
	}
}

func TestDiscoverOutputRefs_LocalOutputRef(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "output", "exp-231"), 0o755); err != nil {
		t.Fatalf("mkdir output dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "output", "exp-231", "summary.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write summary: %v", err)
	}

	files, err := DiscoverOutputRefs(tmpDir, []string{"local:output/exp-231/"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(files), 1; got != want {
		t.Fatalf("file count = %d, want %d: %+v", got, want, files)
	}
	if files[0].RelPath != "output/exp-231/summary.json" {
		t.Fatalf("path = %q, want output/exp-231/summary.json", files[0].RelPath)
	}
}
