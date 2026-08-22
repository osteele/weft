package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
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
	if strings.Contains(torchPreflightPythonCode, "torch.cuda.init() if torch.cuda.is_available() else None") {
		t.Fatal("preflight must not skip CUDA initialization when torch reports CUDA unavailable")
	}
	for _, want := range []string{
		torchPreflightPythonStartedMarker,
		torchPreflightTorchImportedMarker,
		"not torch.cuda.is_available()",
		"torch.cuda.init()",
		"torch.zeros(1, device='cuda')",
		"torch.cuda.synchronize()",
	} {
		if !strings.Contains(torchPreflightPythonCode, want) {
			t.Fatalf("preflight code %q missing %q", torchPreflightPythonCode, want)
		}
	}
}

func TestTorchPreflightFailureStage(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   TorchPreflightFailureStage
	}{
		{
			name:   "resolver fails before Python starts",
			output: "error: no solution found when resolving dependencies",
			want:   TorchPreflightFailureEnvironment,
		},
		{
			name:   "resolver quotes command containing markers",
			output: "failed to run python -c \"print('" + torchPreflightPythonStartedMarker + "')\"",
			want:   TorchPreflightFailureEnvironment,
		},
		{
			name:   "torch import fails",
			output: torchPreflightPythonStartedMarker + "\nModuleNotFoundError: No module named 'torch'",
			want:   TorchPreflightFailureTorchImport,
		},
		{
			name: "CUDA probe fails after torch import",
			output: torchPreflightPythonStartedMarker + "\n" +
				torchPreflightTorchImportedMarker + "\ntorch.cuda.is_available() is false",
			want: TorchPreflightFailureCUDA,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := torchPreflightFailureStage(tt.output); got != tt.want {
				t.Fatalf("torchPreflightFailureStage(%q) = %q, want %q", tt.output, got, tt.want)
			}
		})
	}
}

func TestTorchPreflightFailurePhase(t *testing.T) {
	tests := map[TorchPreflightFailureStage]db.FailurePhase{
		TorchPreflightFailureEnvironment: db.PhaseTorchPreflightEnvironment,
		TorchPreflightFailureTorchImport: db.PhaseTorchPreflightImport,
		TorchPreflightFailureCUDA:        db.PhaseTorchPreflightCUDA,
	}
	for stage, want := range tests {
		if got := torchPreflightFailurePhase(stage); got != want {
			t.Fatalf("torchPreflightFailurePhase(%q) = %q, want %q", stage, got, want)
		}
	}
}

func TestTorchPreflightCommandUsesUVRunWithLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	got := torchPreflightShellCommand(dir, "uv run train.py")
	if !strings.HasPrefix(got, "uv run --no-sync python -c ") {
		t.Fatalf("command = %q, want uv-managed python", got)
	}
}

func TestTorchPreflightCommandFallsBackToPython3WithoutLock(t *testing.T) {
	got := torchPreflightShellCommand(t.TempDir(), "python train.py")
	for _, want := range []string{"command -v python3", "python3 -c", "neither python nor python3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("command = %q, missing %q", got, want)
		}
	}
	if strings.HasPrefix(got, "python -c ") {
		t.Fatalf("command = %q, want interpreter fallback instead of bare python", got)
	}
}

func TestTorchPreflightCommandUsesScriptDependencies(t *testing.T) {
	dir := t.TempDir()
	script := `# /// script
# dependencies = ["torch>=2.2", "transformers>=4.44"]
# ///
import torch
`
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	got := torchPreflightShellCommand(dir, "uv run train.py")
	for _, want := range []string{
		"uv run --isolated",
		"--with 'torch>=2.2'",
		"--with 'transformers>=4.44'",
		"python -c",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("command = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "--no-sync") {
		t.Fatalf("command = %q, script preflight must not use project --no-sync", got)
	}
}

func TestShouldRunTorchPreflightSkipsProjectTorchForScriptImage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pyproject.toml"), `[project]
dependencies = ["torch==2.6.0"]
`)
	writeFile(t, filepath.Join(dir, "profile_sglang.py"), `# /// script
# [tool.weft]
# image = "lmsysorg/sglang:dev-cu13"
# gpu-arch-max = "any"
# ///
print("sglang runtime owns torch")
`)

	meta, err := dataloc.ScanScriptMeta(dir, "uv run profile_sglang.py")
	if err != nil {
		t.Fatal(err)
	}
	if shouldRunTorchPreflight(dir, "uv run profile_sglang.py", meta) {
		t.Fatal("shouldRunTorchPreflight = true, want false for script image runtime")
	}
}

func TestShouldRunTorchPreflightSkipsProjectTorchForProjectRuntimeImage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pyproject.toml"), `[project]
dependencies = ["torch==2.6.0"]
`)
	writeFile(t, filepath.Join(dir, ".weft.toml"), `[cloud]
image = "sglang:dev-cu13"
`)
	writeFile(t, filepath.Join(dir, "profile_sglang.py"), `print("sglang runtime owns torch")
`)

	meta, err := dataloc.ScanScriptMeta(dir, "uv run profile_sglang.py")
	if err != nil {
		t.Fatal(err)
	}
	if shouldRunTorchPreflight(dir, "uv run profile_sglang.py", meta) {
		t.Fatal("shouldRunTorchPreflight = true, want false for project runtime image")
	}
}

func TestShouldRunTorchPreflightSkipsProjectTorchForIsolatedScript(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pyproject.toml"), `[project]
dependencies = ["torch==2.6.0"]
`)
	writeFile(t, filepath.Join(dir, "train.py"), `# /// script
# [tool.weft]
# isolated = true
# ///
print("isolated runtime")
`)

	meta, err := dataloc.ScanScriptMeta(dir, "uv run train.py")
	if err != nil {
		t.Fatal(err)
	}
	if shouldRunTorchPreflight(dir, "uv run train.py", meta) {
		t.Fatal("shouldRunTorchPreflight = true, want false for isolated script runtime")
	}
}

func TestShouldRunTorchPreflightRunsForScriptTorchDependencies(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "train.py"), `# /// script
# dependencies = ["torch>=2.2"]
# [tool.weft]
# image = "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime"
# ///
import torch
`)

	meta, err := dataloc.ScanScriptMeta(dir, "uv run train.py")
	if err != nil {
		t.Fatal(err)
	}
	if !shouldRunTorchPreflight(dir, "uv run train.py", meta) {
		t.Fatal("shouldRunTorchPreflight = false, want true for script torch dependencies")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTorchPreflightCommandBindsScriptRequiresPython(t *testing.T) {
	dir := t.TempDir()
	script := `# /// script
# requires-python = ">=3.11,<3.14"
# dependencies = ["torch>=2.2,<2.7"]
# ///
import torch
`
	writeFile(t, filepath.Join(dir, "screen.py"), script)
	got := torchPreflightShellCommand(dir, "uv run screen.py")
	// Without the bound, uv resolves against the image default interpreter;
	// on an image defaulting to 3.14 that leaves torch<2.7 with no wheel.
	if !strings.Contains(got, `--python '>=3.11,<3.14'`) {
		t.Fatalf("command = %q, want the script requires-python bound", got)
	}
	if !strings.Contains(got, "--with 'torch>=2.2,<2.7'") {
		t.Fatalf("command = %q, want the script dependencies preserved", got)
	}
}

func TestTorchPreflightCommandOmitsPythonWithoutRequiresPython(t *testing.T) {
	dir := t.TempDir()
	script := `# /// script
# dependencies = ["torch>=2.2"]
# ///
import torch
`
	writeFile(t, filepath.Join(dir, "train.py"), script)
	got := torchPreflightShellCommand(dir, "uv run train.py")
	if strings.Contains(got, "--python") {
		t.Fatalf("command = %q, want no interpreter request when the script declares no bound", got)
	}
}
