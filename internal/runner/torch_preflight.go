package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/osteele/weft/internal/dataloc"
)

// torchPreflightCommand is the snippet we run before the user command on
// GPU-using torch jobs. It loads torch, exercises the CUDA initialization
// path that surfaces driver-too-old errors, and exits.
const torchPreflightCommand = `python -c "import torch; torch.cuda.init() if torch.cuda.is_available() else None; print('weft: torch preflight ok')"`

// torchPreflightTimeout caps the preflight at a short wall-clock budget.
// The check itself takes ~1-3s on a healthy host; we allow 30s so cold
// venv interpreter starts don't false-positive as failures.
const torchPreflightTimeout = 30 * time.Second

// runTorchPreflight runs a small Python snippet that imports torch and
// initializes CUDA. On failure, the command's stderr is appended to the
// job log so the post-hoc remediator can classify it. Returns (ExitInfo,
// nil) on success and (ExitInfo, error) on failure or timeout.
//
// The preflight uses `uv run --no-sync` when uv.lock is present so the
// project's resolved torch is exercised (not whatever happens to be on
// the system PATH). Otherwise it falls back to bare `python`.
func runTorchPreflight(jobID int64, workingDir string, envVars []string, paths JobPaths, setupTimeout time.Duration) (ExitInfo, error) {
	deadline := torchPreflightTimeout
	if setupTimeout > 0 && setupTimeout < deadline {
		deadline = setupTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	cmdStr := torchPreflightCommand
	if dataloc.HasUVLock(workingDir) {
		// `uv run` arranges the project's resolved interpreter without
		// re-running setup. --no-sync mirrors prepareUVRunCommand.
		cmdStr = `uv run --no-sync ` + cmdStr
	}
	appendSetupLog(paths.Log, []byte("weft: torch preflight: "+cmdStr+"\n"))

	cmd := exec.CommandContext(ctx, "bash", "-lc", cmdStr)
	cmd.Dir = workingDir
	cmd.Env = envVars
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
