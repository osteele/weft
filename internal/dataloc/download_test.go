package dataloc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildHFDownloadCommand_Model(t *testing.T) {
	cmd, err := buildHFDownloadCommand(DataAsset{Kind: AssetHFModel, ID: "meta-llama/Llama-3-8B"}, "main")
	if err != nil {
		t.Fatalf("buildHFDownloadCommand: %v", err)
	}
	if !strings.Contains(cmd, "HF_HUB_CACHE") || !strings.Contains(cmd, "HF_HOME") {
		t.Fatalf("command should resolve HF_HUB_CACHE/HF_HOME: %s", cmd)
	}
	if !strings.Contains(cmd, `if [ -z "${HOME:-}" ]`) {
		t.Fatalf("command should establish HOME before building PATH: %s", cmd)
	}
	if !strings.Contains(cmd, "$HOME/.local/share/mise/shims") {
		t.Fatalf("command should include mise shims in PATH: %s", cmd)
	}
	if !strings.Contains(cmd, "$HOME/.local/share/mise/installs/python/latest/bin") {
		t.Fatalf("command should include mise Python install bin in PATH: %s", cmd)
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
	if !strings.Contains(cmd, "uv tool install 'huggingface-hub[hf_xet]'") {
		t.Fatalf("command missing remote helper bootstrap via uv: %s", cmd)
	}
	if !strings.Contains(cmd, "python3 -m pip install --user --quiet 'huggingface-hub[hf_xet]'") {
		t.Fatalf("command missing remote helper bootstrap via pip: %s", cmd)
	}
	if strings.Count(cmd, "_hfdl='hf download'") < 2 {
		t.Fatalf("command should rediscover hf after helper bootstrap: %s", cmd)
	}
	if !strings.Contains(cmd, "meta-llama/Llama-3-8B") {
		t.Fatalf("command missing repo id: %s", cmd)
	}
	// FR1: skip native-checkpoint dirs for model repos.
	if !strings.Contains(cmd, "--exclude 'original/*'") {
		t.Fatalf("model download missing native-checkpoint exclude: %s", cmd)
	}
	if !strings.Contains(cmd, `ignore_patterns=["original/*"]`) {
		t.Fatalf("model snapshot_download missing ignore_patterns: %s", cmd)
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
	// FR1: datasets keep their full snapshot — no native-checkpoint exclude.
	if strings.Contains(cmd, "original/") {
		t.Fatalf("dataset download should not exclude original/: %s", cmd)
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

// detachedDownloadMock returns a hostCommandRunner that models the spawn/poll
// flow of runDetachedRemoteCommand. The download is reported as having
// finished with exitCode and the given captured stderr on the first poll.
// df queries (from checkHFCacheFreeSpace) are still answered with abundant
// free space.
func detachedDownloadMock(t *testing.T, exitCode int, capturedStderr string) commandRunnerFunc {
	t.Helper()
	// Speed up the polling so tests don't pay 5s per assertion.
	origInterval := detachedPollInterval
	detachedPollInterval = 1 * time.Millisecond
	t.Cleanup(func() { detachedPollInterval = origInterval })
	return func(_ context.Context, _ string, command string) (string, string, error) {
		switch {
		case strings.Contains(command, "df -Pk"):
			return "123456789\n", "", nil
		case strings.Contains(command, "nohup bash -c") && strings.Contains(command, "cmd.sh"):
			// Spawn succeeded.
			return "OK\n", "", nil
		case strings.Contains(command, `if [ -f "$D/status" ]`):
			// Poll: report completion with captured stderr.
			return fmt.Sprintf("STATUS=%d\n---STDERR---\n%s", exitCode, capturedStderr), "", nil
		default:
			return "", "", fmt.Errorf("unexpected command in test mock: %s", command)
		}
	}
}

func TestDownloadAssetToHost_RepoNotFoundMessageIsActionable(t *testing.T) {
	origHostRunner := hostCommandRunner
	t.Cleanup(func() { hostCommandRunner = origHostRunner })

	hostCommandRunner = detachedDownloadMock(t, 1,
		"Error: Repository not found.\nCheck the `repo_id` and `repo_type` parameters.\n")

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

	hostCommandRunner = detachedDownloadMock(t, 1, "first line\nsecond line\n")

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

	// In real life the hf CLI surfaces "No local file found. Retrying..."
	// on stderr (stdout is /dev/null in buildHFDownloadCommand). The
	// detached runner captures stderr from the remote stderr.log; that's
	// what reaches formatHFDownloadError's stderr argument.
	hostCommandRunner = detachedDownloadMock(t, 1,
		"Fetching 52 files...\nNo local file found. Retrying...\n")

	_, err := DownloadAssetToHost(context.Background(), "cool30", DataAsset{Kind: AssetHFDataset, ID: "DKYoon/SlimPajama-6B"}, "main")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Hugging Face retried missing files") {
		t.Fatalf("expected retry-loop diagnosis, got: %q", msg)
	}
	if !strings.Contains(msg, "No local file found") {
		t.Fatalf("expected stderr to be included, got: %q", msg)
	}
}

func TestDownloadAssetToHost_FinalCacheScanUsesCallerContext(t *testing.T) {
	origHostRunner := hostCommandRunner
	t.Cleanup(func() { hostCommandRunner = origHostRunner })
	origInterval := detachedPollInterval
	detachedPollInterval = 1 * time.Millisecond
	t.Cleanup(func() { detachedPollInterval = origInterval })

	var scanSawDeadline bool
	hostCommandRunner = func(ctx context.Context, _ string, command string) (string, string, error) {
		switch {
		case strings.Contains(command, "df -Pk"):
			return "123456789\n", "", nil
		case strings.Contains(command, "nohup bash -c") && strings.Contains(command, "cmd.sh"):
			return "OK\n", "", nil
		case strings.Contains(command, `if [ -f "$D/status" ]`):
			return "STATUS=0\n---STDERR---\n", "", nil
		case strings.Contains(command, "_dirs=()") && strings.Contains(command, "du -sb"):
			if _, ok := ctx.Deadline(); ok {
				scanSawDeadline = true
			}
			return "2048\tok\t/home/test/.cache/huggingface/hub/models--gpt2\n", "", nil
		default:
			return "", "", fmt.Errorf("unexpected command in test mock: %s", command)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := DownloadAssetToHost(ctx, "cool30", DataAsset{Kind: AssetHFModel, ID: "gpt2"}, "main")
	if err != nil {
		t.Fatalf("DownloadAssetToHost: %v", err)
	}
	if !scanSawDeadline {
		t.Fatal("expected final HF cache scan to inherit caller context deadline")
	}
}

func TestDownloadAssetToHost_ReportsIncompleteCacheAfterSuccessfulDownload(t *testing.T) {
	origHostRunner := hostCommandRunner
	t.Cleanup(func() { hostCommandRunner = origHostRunner })
	origInterval := detachedPollInterval
	detachedPollInterval = 1 * time.Millisecond
	t.Cleanup(func() { detachedPollInterval = origInterval })

	hostCommandRunner = func(_ context.Context, _ string, command string) (string, string, error) {
		switch {
		case strings.Contains(command, "df -Pk"):
			return "123456789\n", "", nil
		case strings.Contains(command, "nohup bash -c") && strings.Contains(command, "cmd.sh"):
			return "OK\n", "", nil
		case strings.Contains(command, `if [ -f "$D/status" ]`):
			return "STATUS=0\n---STDERR---\n", "", nil
		case strings.Contains(command, "_dirs=()") && strings.Contains(command, "du -sb"):
			return "2276341\tincomplete\t/home/test/.cache/huggingface/hub/models--meta-llama--Llama-3.1-8B-Instruct\n", "", nil
		default:
			return "", "", fmt.Errorf("unexpected command in test mock: %s", command)
		}
	}

	_, err := DownloadAssetToHost(context.Background(), "cool30", DataAsset{Kind: AssetHFModel, ID: "meta-llama/Llama-3.1-8B-Instruct"}, "main")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cache entry on cool30 is incomplete after download") {
		t.Fatalf("unexpected error: %q", msg)
	}
	if strings.Contains(msg, "downloaded on cool30 but not found") {
		t.Fatalf("error should not use misleading not-found wording: %q", msg)
	}
}

// TestRunDetachedRemoteCommand_CancelKillsRemote verifies that when the
// caller's context is cancelled mid-poll, the helper sends a SIGTERM to
// the remote PID before returning ctx.Err(). Without this, weft data fetch
// orphans nohup'd processes when the user hits Ctrl-C.
func TestRunDetachedRemoteCommand_CancelKillsRemote(t *testing.T) {
	origHostRunner := hostCommandRunner
	t.Cleanup(func() { hostCommandRunner = origHostRunner })
	origInterval := detachedPollInterval
	detachedPollInterval = 1 * time.Millisecond
	t.Cleanup(func() { detachedPollInterval = origInterval })

	var killed bool
	hostCommandRunner = func(ctx context.Context, _ string, command string) (string, string, error) {
		switch {
		case strings.Contains(command, "nohup bash -c"):
			return "OK\n", "", nil
		case strings.Contains(command, "kill -TERM"):
			killed = true
			return "", "", nil
		case strings.Contains(command, `if [ -f "$D/status" ]`):
			return "RUNNING\n", "", nil
		}
		return "", "", fmt.Errorf("unexpected command: %s", command)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _, err := runDetachedRemoteCommand(ctx, "cool30", "sleep 30")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if !killed {
		t.Fatal("expected SIGTERM to be sent to remote PID on context cancel")
	}
}
