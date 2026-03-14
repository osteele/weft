package dataloc

import (
	"context"
	"strings"
	"testing"
)

func TestBuildHFDownloadCommand_Model(t *testing.T) {
	cmd, err := buildHFDownloadCommand(DataAsset{Kind: AssetHFModel, ID: "meta-llama/Llama-3-8B"}, "main")
	if err != nil {
		t.Fatalf("buildHFDownloadCommand: %v", err)
	}
	if !strings.Contains(cmd, "_hfdl=hf_xet") {
		t.Fatalf("command missing hf_xet candidate: %s", cmd)
	}
	if !strings.Contains(cmd, "_hfdl=hf") {
		t.Fatalf("command missing hf candidate: %s", cmd)
	}
	if !strings.Contains(cmd, "$_hfdl download --repo-type model") {
		t.Fatalf("command missing hf cli dispatch: %s", cmd)
	}
	if !strings.Contains(cmd, "snapshot_download") {
		t.Fatalf("command missing python fallback: %s", cmd)
	}
	if !strings.Contains(cmd, "meta-llama/Llama-3-8B") {
		t.Fatalf("command missing repo id: %s", cmd)
	}
}

func TestBuildHFDownloadCommand_Dataset(t *testing.T) {
	cmd, err := buildHFDownloadCommand(DataAsset{Kind: AssetHFDataset, ID: "HuggingFaceFW/fineweb"}, "refs/pr/1")
	if err != nil {
		t.Fatalf("buildHFDownloadCommand: %v", err)
	}
	if !strings.Contains(cmd, "--repo-type dataset") {
		t.Fatalf("command missing dataset repo type: %s", cmd)
	}
	if !strings.Contains(cmd, "refs/pr/1") {
		t.Fatalf("command missing revision: %s", cmd)
	}
}

func TestBuildHFDownloadCommand_RejectsUnsupportedKinds(t *testing.T) {
	if _, err := buildHFDownloadCommand(DataAsset{Kind: AssetCheckpoint, ID: "ckpt"}, "main"); err == nil {
		t.Fatal("expected unsupported asset kind to fail")
	}
}

func TestRunHostCommand_LocalhostUsesLocalRunner(t *testing.T) {
	orig := localCommandRunner
	t.Cleanup(func() { localCommandRunner = orig })

	called := false
	localCommandRunner = func(ctx context.Context, command string) (string, string, error) {
		called = true
		if command != "echo local" {
			t.Fatalf("command = %q, want %q", command, "echo local")
		}
		return "ok", "", nil
	}

	stdout, stderr, err := hostCommandRunner(context.Background(), "localhost", "echo local")
	if err != nil {
		t.Fatalf("hostCommandRunner: %v", err)
	}
	if !called {
		t.Fatal("expected local command runner to be used")
	}
	if stdout != "ok" || stderr != "" {
		t.Fatalf("unexpected outputs stdout=%q stderr=%q", stdout, stderr)
	}
}
