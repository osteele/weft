package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRepeatedLifecycleHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[[events.hooks]]
id = "agent-review"
command = ["agent-review", "daemon", "hook"]

[[events.hooks]]
id = "audit"
command = ["event-audit", "--stdin"]
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetConfigPathsForTesting(path, ""))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Events.Hooks) != 2 {
		t.Fatalf("hooks = %d, want 2", len(cfg.Events.Hooks))
	}
	if cfg.Events.Hooks[0].ID != "agent-review" || len(cfg.Events.Hooks[0].Command) != 3 || cfg.Events.Hooks[1].ID != "audit" {
		t.Fatalf("hooks = %+v", cfg.Events.Hooks)
	}
}
