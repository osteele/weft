package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProjectConfig_Found(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, ProjectConfigFile)
	content := `sync:
  extra_paths:
    - ~/sources/vidur/data/profiling/compute/
    - ~/data/calibration/
`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if len(cfg.Sync.ExtraPaths) != 2 {
		t.Fatalf("got %d extra_paths, want 2", len(cfg.Sync.ExtraPaths))
	}
	if cfg.Sync.ExtraPaths[0] != "~/sources/vidur/data/profiling/compute/" {
		t.Errorf("extra_paths[0] = %q, want ~/sources/vidur/data/profiling/compute/", cfg.Sync.ExtraPaths[0])
	}
}

func TestLoadProjectConfig_WalkUp(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ProjectConfigFile)
	content := `sync:
  extra_paths:
    - ~/data/
`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a subdirectory
	subdir := filepath.Join(root, "src", "pkg")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadProjectConfig(subdir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config from parent directory")
	}
	if len(cfg.Sync.ExtraPaths) != 1 {
		t.Fatalf("got %d extra_paths, want 1", len(cfg.Sync.ExtraPaths))
	}
}

func TestLoadProjectConfig_NotFound(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadProjectConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Errorf("expected nil config when no .weft.yaml exists, got %v", cfg)
	}
}

func TestLoadProjectConfig_EmptyDir(t *testing.T) {
	cfg, err := LoadProjectConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Errorf("expected nil config for empty dir, got %v", cfg)
	}
}
