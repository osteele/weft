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

func TestProjectOutputsConfig_EffectiveDirs(t *testing.T) {
	t.Run("empty config returns defaults", func(t *testing.T) {
		cfg := ProjectOutputsConfig{}
		dirs := cfg.EffectiveDirs()
		if len(dirs) != 2 || dirs[0] != "output/" || dirs[1] != "outputs/" {
			t.Errorf("unexpected default dirs: %v", dirs)
		}
	})

	t.Run("custom dirs override defaults", func(t *testing.T) {
		cfg := ProjectOutputsConfig{Dirs: []string{"results/"}}
		dirs := cfg.EffectiveDirs()
		if len(dirs) != 1 || dirs[0] != "results/" {
			t.Errorf("unexpected dirs: %v", dirs)
		}
	})
}

func TestProjectOutputsConfig_EffectiveMaxAutoSyncMB(t *testing.T) {
	t.Run("zero returns default", func(t *testing.T) {
		cfg := ProjectOutputsConfig{}
		if mb := cfg.EffectiveMaxAutoSyncMB(); mb != 100 {
			t.Errorf("expected 100, got %d", mb)
		}
	})

	t.Run("custom value used", func(t *testing.T) {
		cfg := ProjectOutputsConfig{MaxAutoSyncMB: 50}
		if mb := cfg.EffectiveMaxAutoSyncMB(); mb != 50 {
			t.Errorf("expected 50, got %d", mb)
		}
	})
}

func TestProjectOutputDirs(t *testing.T) {
	t.Run("no config file returns defaults", func(t *testing.T) {
		tmpDir := t.TempDir()
		dirs := ProjectOutputDirs(tmpDir)
		if len(dirs) != 2 {
			t.Errorf("expected 2 default dirs, got %d", len(dirs))
		}
	})

	t.Run("config with outputs section", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgContent := "outputs:\n  dirs:\n    - results/\n  max_auto_sync_mb: 200\n"
		os.WriteFile(filepath.Join(tmpDir, ".weft.yaml"), []byte(cfgContent), 0644)

		dirs := ProjectOutputDirs(tmpDir)
		if len(dirs) != 1 || dirs[0] != "results/" {
			t.Errorf("unexpected dirs: %v", dirs)
		}

		mb := ProjectMaxAutoSyncMB(tmpDir)
		if mb != 200 {
			t.Errorf("expected 200, got %d", mb)
		}
	})

	t.Run("empty dir returns defaults", func(t *testing.T) {
		dirs := ProjectOutputDirs("")
		if len(dirs) != 2 {
			t.Errorf("expected 2 default dirs, got %d", len(dirs))
		}
	})
}
