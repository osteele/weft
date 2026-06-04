package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/spf13/cobra"
)

func TestProviderStatusRowsLegacyDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	rows := providerStatusRows(cfg)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].Provider != string(cloud.ProviderVastai) || !rows[0].Active || rows[0].Config != "auto" {
		t.Fatalf("vastai row = %+v", rows[0])
	}
	if rows[1].Provider != string(cloud.ProviderRunpod) || rows[1].Active || rows[1].Config != "auto" {
		t.Fatalf("runpod row = %+v", rows[1])
	}
}

func TestRunProviderSetWritesAndResetsConfig(t *testing.T) {
	dir := t.TempDir()
	restore := config.SetConfigPathsForTesting(filepath.Join(dir, "config.toml"), filepath.Join(dir, "config.yaml"))
	defer restore()

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runProviderSet(cmd, "runpod", config.Bool(true), "enabled"); err != nil {
		t.Fatalf("runProviderSet enable: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ProviderExplicitlyEnabled(cloud.ProviderRunpod) {
		t.Fatal("runpod should be enabled")
	}

	if err := runProviderSet(cmd, "runpod", nil, "reset to auto"); err != nil {
		t.Fatalf("runProviderSet reset: %v", err)
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("Load after reset: %v", err)
	}
	setting, err := cfg.ProviderEnabledSetting(cloud.ProviderRunpod)
	if err != nil {
		t.Fatalf("ProviderEnabledSetting: %v", err)
	}
	if setting != nil {
		t.Fatalf("runpod enabled setting = %v, want nil", *setting)
	}
	data, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "enabled") {
		t.Fatalf("config still contains enabled key after reset:\n%s", string(data))
	}
}
