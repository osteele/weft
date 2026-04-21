package runpod

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunpodctlCommandEnvUsesExistingEnvKey(t *testing.T) {
	t.Setenv("RUNPOD_API_KEY", "from-env")
	env := runpodctlCommandEnv()
	if !envContainsKV(env, "RUNPOD_API_KEY", "from-env") {
		t.Fatalf("expected RUNPOD_API_KEY from environment, env=%v", env)
	}
}

func TestRunpodctlCommandEnvLoadsKeyFromConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("RUNPOD_API_KEY", "")
	runpodDir := filepath.Join(tmp, ".runpod")
	if err := os.MkdirAll(runpodDir, 0o755); err != nil {
		t.Fatalf("mkdir .runpod: %v", err)
	}
	configPath := filepath.Join(runpodDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[default]\napi_key = \"from-config\"\n"), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	env := runpodctlCommandEnv()
	if !envContainsKV(env, "RUNPOD_API_KEY", "from-config") {
		t.Fatalf("expected RUNPOD_API_KEY from ~/.runpod/config.toml, env=%v", env)
	}
}

func envContainsKV(env []string, key, value string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if strings.TrimPrefix(entry, prefix) == value {
				return true
			}
		}
	}
	return false
}
