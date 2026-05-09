package dataloc

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBuildHFDownloadCommand_Model(t *testing.T) {
	cmd, err := buildHFDownloadCommand(DataAsset{Kind: AssetHFModel, ID: "meta-llama/Llama-3-8B"}, "main")
	if err != nil {
		t.Fatalf("buildHFDownloadCommand: %v", err)
	}
	if !strings.Contains(cmd, "HF_HUB_CACHE") || !strings.Contains(cmd, "HF_HOME") {
		t.Fatalf("command should resolve HF_HUB_CACHE/HF_HOME: %s", cmd)
	}
	if !strings.Contains(cmd, "_hfdl='hf_xet download'") {
		t.Fatalf("command missing hf_xet candidate: %s", cmd)
	}
	if !strings.Contains(cmd, "_hfdl='hf download'") {
		t.Fatalf("command missing hf candidate: %s", cmd)
	}
	if !strings.Contains(cmd, "_hfdl=hf-download") {
		t.Fatalf("command missing hf-download candidate: %s", cmd)
	}
	if !strings.Contains(cmd, "$_hfdl --repo-type model") {
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

func TestDownloadAssetToHost_RepoNotFoundMessageIsActionable(t *testing.T) {
	origHostRunner := hostCommandRunner
	t.Cleanup(func() { hostCommandRunner = origHostRunner })

	hostCommandRunner = func(_ context.Context, _ string, command string) (string, string, error) {
		if strings.Contains(command, "df -Pk") {
			return "123456789\n", "", nil
		}
		return "", "Error: Repository not found.\nCheck the `repo_id` and `repo_type` parameters.\n", errors.New("exit status 1")
	}

	_, err := DownloadAssetToHost(context.Background(), "cool30", DataAsset{Kind: AssetHFModel, ID: "application/json"}, "main")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Hugging Face model repo \"application/json\" not found") {
		t.Fatalf("unexpected error message: %q", msg)
	}
	if !strings.Contains(msg, "update the job input") {
		t.Fatalf("expected remediation guidance in message: %q", msg)
	}
	if strings.Contains(msg, "repo_id") || strings.Contains(msg, "repo_type") {
		t.Fatalf("message leaked low-level huggingface_hub args: %q", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Fatalf("message contains newlines: %q", msg)
	}
}

func TestDownloadAssetToHost_GenericStderrIsNormalized(t *testing.T) {
	origHostRunner := hostCommandRunner
	t.Cleanup(func() { hostCommandRunner = origHostRunner })

	hostCommandRunner = func(_ context.Context, _ string, command string) (string, string, error) {
		if strings.Contains(command, "df -Pk") {
			return "123456789\n", "", nil
		}
		return "", "first line\nsecond line\n", errors.New("exit status 1")
	}

	_, err := DownloadAssetToHost(context.Background(), "cool30", DataAsset{Kind: AssetHFModel, ID: "gpt2"}, "main")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "first line second line") {
		t.Fatalf("stderr should be normalized to single line: %q", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Fatalf("message contains newlines: %q", msg)
	}
}

func TestDownloadAssetToHost_IncludesStdoutRetryLoop(t *testing.T) {
	origHostRunner := hostCommandRunner
	t.Cleanup(func() { hostCommandRunner = origHostRunner })

	hostCommandRunner = func(_ context.Context, _ string, command string) (string, string, error) {
		if strings.Contains(command, "df -Pk") {
			return "123456789\n", "", nil
		}
		return "Fetching 52 files...\nNo local file found. Retrying...\n", "", errors.New("exit status 1")
	}

	_, err := DownloadAssetToHost(context.Background(), "cool30", DataAsset{Kind: AssetHFDataset, ID: "DKYoon/SlimPajama-6B"}, "main")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Hugging Face retried missing files") {
		t.Fatalf("expected retry-loop diagnosis, got: %q", msg)
	}
	if !strings.Contains(msg, "No local file found") {
		t.Fatalf("expected stdout to be included, got: %q", msg)
	}
}
