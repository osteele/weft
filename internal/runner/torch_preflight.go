package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/osteele/weft/internal/dataloc"
)

// torchPreflightPythonCode is the snippet we run before the user command on
// GPU-using torch jobs. It requires CUDA to be available, initializes it, and
// performs a tiny device allocation so driver/runtime mismatches fail before
// the user script can silently fall back to CPU.
const torchPreflightPythonCode = `import sys, torch; sys.exit('torch.cuda.is_available() is false') if not torch.cuda.is_available() else None; torch.cuda.init(); torch.zeros(1, device='cuda').sum().item(); torch.cuda.synchronize(); print('weft: torch preflight ok')`

const torchPreflightCommand = `python -c "` + torchPreflightPythonCode + `"`

// torchPreflightTimeout caps the preflight at a short wall-clock budget.
// The check itself takes ~1-3s on a healthy host; we allow 30s so cold
// venv interpreter starts don't false-positive as failures.
const torchPreflightTimeout = 30 * time.Second

// preflightEnv builds the env for the `bash -lc` preflight invocation. It
// merges os.Environ() under the job's envVars so HOME (and other
// parent-process env) reaches the login shell. Without HOME, bash -lc
// cannot resolve ~/.profile, the snippet that adds ~/.local/bin to PATH
// never runs, and `uv` — installed at $HOME/.local/bin/uv on the cloud
// pytorch image — is unreachable, producing a spurious exit 127 even
// though uv is on disk. Job-specified env still overrides parent env
// (mergeEnvVars is last-writer-wins, and envVars is the overlay).
func preflightEnv(envVars []string) []string {
	return mergeEnvVars(os.Environ(), envVars)
}

// runTorchPreflight runs a small Python snippet that imports torch and
// initializes CUDA. On failure, the command's stderr is appended to the
// job log so the post-hoc remediator can classify it. Returns (ExitInfo,
// nil) on success and (ExitInfo, error) on failure or timeout.
//
// The preflight uses `uv run --no-sync` when uv.lock is present so the
// project's resolved torch is exercised (not whatever happens to be on
// the system PATH). Otherwise it uses the image interpreter, accepting
// either `python` or `python3` because several cloud base images ship only
// the latter.
func runTorchPreflight(jobID int64, workingDir, jobCommand string, envVars []string, paths JobPaths, setupTimeout time.Duration) (ExitInfo, error) {
	deadline := torchPreflightTimeout
	if setupTimeout > 0 && setupTimeout < deadline {
		deadline = setupTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	cmdStr := torchPreflightShellCommand(workingDir, jobCommand)
	appendSetupLog(paths.Log, []byte("weft: torch preflight: "+cmdStr+"\n"))

	cmd := exec.CommandContext(ctx, "bash", "-lc", cmdStr)
	cmd.Dir = workingDir
	cmd.Env = preflightEnv(envVars)
	logFile, openErr := os.OpenFile(paths.Log, os.O_APPEND|os.O_WRONLY, 0o644)
	if openErr == nil {
		defer logFile.Close()
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	runErr := cmd.Run()
	if runErr == nil {
		return ExitInfo{}, nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		// A timed-out preflight is suspicious but not by itself proof of
		// driver mismatch — treat it as advisory and let the user command
		// run. Surface it in the log and continue.
		appendSetupLog(paths.Log, []byte(fmt.Sprintf("weft: torch preflight timed out after %s; continuing\n", deadline)))
		return ExitInfo{}, nil
	}
	ei := ExtractExitInfo(runErr)
	if ei.ExitCode == 0 {
		ei.ExitCode = 1
	}
	return ei, fmt.Errorf("torch preflight failed (exit %d): %w", ei.ExitCode, runErr)
}

func torchPreflightShellCommand(workingDir, jobCommand string) string {
	if scriptDeps := torchPreflightScriptDeps(workingDir, jobCommand); len(scriptDeps) > 0 {
		var b strings.Builder
		b.WriteString("uv run --isolated")
		for _, dep := range scriptDeps {
			b.WriteString(" --with ")
			b.WriteString(shellQuote(dep))
		}
		b.WriteString(" ")
		b.WriteString(torchPreflightCommand)
		return b.String()
	}
	if dataloc.HasUVLock(workingDir) {
		// `uv run` arranges the project's resolved interpreter without
		// re-running setup. --no-sync is set explicitly here in case the
		// preflight runs before uvRunEnvAdditions has appended UV_NO_SYNC=1
		// to the job env; the two are redundant when both are present.
		return `uv run --no-sync ` + torchPreflightCommand
	}
	return `if command -v python >/dev/null 2>&1; then python -c "` + torchPreflightPythonCode + `"; elif command -v python3 >/dev/null 2>&1; then python3 -c "` + torchPreflightPythonCode + `"; else echo "weft: torch preflight: neither python nor python3 found" >&2; exit 127; fi`
}

func torchPreflightScriptDeps(workingDir, jobCommand string) []string {
	deps := dataloc.ScanScriptDependencies(workingDir, jobCommand)
	if len(deps) == 0 {
		return nil
	}
	parsed := dataloc.ParseDepSpecs(deps)
	if !dataloc.ScriptUsesTorch(workingDir, jobCommand) && dataloc.LibraryMinCUDAFromDeps(parsed) == "" {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		dep = strings.TrimSpace(dep)
		if dep != "" {
			out = append(out, dep)
		}
	}
	return out
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
