package runner

import (
	"strings"
	"testing"
)

// TestPreflightEnv_InheritsHomeFromOsEnviron is a regression test for the
// 2026-05-28 incident in which cloud jobs failed with `uv: command not found`.
// runTorchPreflight invoked `uv run --no-sync …` via `bash -lc` but assigned
// cmd.Env = envVars without merging os.Environ. The job's constructed envVars
// does not carry HOME, so the login shell could not source ~/.profile (the
// snippet that adds ~/.local/bin to PATH), and `uv` — installed at
// $HOME/.local/bin/uv on the cloud pytorch image — was unreachable, even
// though uv was on disk. See `weft: torch preflight: uv run --no-sync …` →
// exit 127 in the cloud logs for wj2221–wj2232 on wi3238/wi3244/wi3245.
//
// preflightEnv must merge os.Environ() in so HOME reaches the login shell.
func TestPreflightEnv_InheritsHomeFromOsEnviron(t *testing.T) {
	t.Setenv("HOME", "/regression/home")
	// envVars omits HOME, simulating the cloud-job env (dotenv + job.Env +
	// artifact env). Without merging os.Environ in, bash -lc cannot resolve
	// ~/.profile and `uv` at $HOME/.local/bin/uv becomes unreachable.
	got := preflightEnv([]string{"WEFT_JOB_ID=42"})
	assertEnvVar(t, got, "HOME", "/regression/home")
	assertEnvVar(t, got, "WEFT_JOB_ID", "42")
}

// TestPreflightEnv_JobEnvOverridesParent ensures that explicit job-level env
// (the overlay) still wins over the parent process env, matching the
// last-writer-wins contract mergeEnvVars documents.
func TestPreflightEnv_JobEnvOverridesParent(t *testing.T) {
	t.Setenv("HF_HOME", "/parent/hf")
	assertEnvVar(t, preflightEnv([]string{"HF_HOME=/workspace/.cache/huggingface"}),
		"HF_HOME", "/workspace/.cache/huggingface")
}

func TestTorchPreflightCommandRequiresWorkingCUDA(t *testing.T) {
	if strings.Contains(torchPreflightCommand, "torch.cuda.init() if torch.cuda.is_available() else None") {
		t.Fatal("preflight must not skip CUDA initialization when torch reports CUDA unavailable")
	}
	for _, want := range []string{
		"not torch.cuda.is_available()",
		"torch.cuda.init()",
		"torch.zeros(1, device='cuda')",
		"torch.cuda.synchronize()",
	} {
		if !strings.Contains(torchPreflightCommand, want) {
			t.Fatalf("preflight command %q missing %q", torchPreflightCommand, want)
		}
	}
}
