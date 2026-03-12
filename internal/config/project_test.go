package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProjectConfig_Found(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, ProjectConfigFile)
	content := `[sync]
extra_paths = ["~/sources/vidur/data/profiling/compute/", "~/data/calibration/"]
exclude_dirs = ["data"]
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
	if len(cfg.Sync.ExcludeDirs) != 1 || cfg.Sync.ExcludeDirs[0] != "data" {
		t.Errorf("exclude_dirs = %v, want [data]", cfg.Sync.ExcludeDirs)
	}
}

func TestLoadProjectConfig_WalkUp(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ProjectConfigFile)
	content := `[sync]
extra_paths = ["~/data/"]
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
		t.Errorf("expected nil config when no project config exists, got %v", cfg)
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

func TestProjectInputs(t *testing.T) {
	t.Run("no config file returns nil", func(t *testing.T) {
		tmpDir := t.TempDir()
		inputs := ProjectInputs(tmpDir)
		if inputs != nil {
			t.Errorf("expected nil, got %v", inputs)
		}
	})

	t.Run("empty dir returns nil", func(t *testing.T) {
		inputs := ProjectInputs("")
		if inputs != nil {
			t.Errorf("expected nil, got %v", inputs)
		}
	})

	t.Run("config with inputs", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgContent := "inputs = [\"hf:gpt2\", \"hf:meta-llama/Llama-3.1-8B\"]\n"
		os.WriteFile(filepath.Join(tmpDir, ".weft.toml"), []byte(cfgContent), 0644)

		inputs := ProjectInputs(tmpDir)
		if len(inputs) != 2 {
			t.Fatalf("expected 2 inputs, got %d", len(inputs))
		}
		if inputs[0] != "hf:gpt2" {
			t.Errorf("inputs[0] = %q, want %q", inputs[0], "hf:gpt2")
		}
		if inputs[1] != "hf:meta-llama/Llama-3.1-8B" {
			t.Errorf("inputs[1] = %q, want %q", inputs[1], "hf:meta-llama/Llama-3.1-8B")
		}
	})

	t.Run("config without inputs returns nil", func(t *testing.T) {
		tmpDir := t.TempDir()
		cfgContent := "[sync]\nextra_paths = [\"~/data/\"]\n"
		os.WriteFile(filepath.Join(tmpDir, ".weft.toml"), []byte(cfgContent), 0644)

		inputs := ProjectInputs(tmpDir)
		if inputs != nil {
			t.Errorf("expected nil, got %v", inputs)
		}
	})
}

func TestProjectExcludeDirs(t *testing.T) {
	tmpDir := t.TempDir()
	cfgContent := "[sync]\nexclude_dirs = [\"data\", \"artifacts\"]\n"
	if err := os.WriteFile(filepath.Join(tmpDir, ".weft.toml"), []byte(cfgContent), 0644); err != nil {
		t.Fatal(err)
	}

	excludes := ProjectExcludeDirs(tmpDir)
	if len(excludes) != 2 {
		t.Fatalf("expected 2 exclude dirs, got %d", len(excludes))
	}
	if excludes[0] != "data" || excludes[1] != "artifacts" {
		t.Fatalf("unexpected exclude dirs: %v", excludes)
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
		cfgContent := "[outputs]\ndirs = [\"results/\"]\nmax_auto_sync_mb = 200\n"
		os.WriteFile(filepath.Join(tmpDir, ".weft.toml"), []byte(cfgContent), 0644)

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
