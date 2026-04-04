package runner

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/inventory"
)

const direnvSetupCommand = `direnv allow && eval "$(direnv export bash)"`

// DetectSetupCommand checks for environment manager marker files in the
// working directory and returns the appropriate setup command to run before
// the job command. Returns "" if no environment manager is detected.
//
// The workingDir should already have ~ expanded.
//
// Detection order (first match wins):
//  1. pyproject.toml + (uv.lock or .venv/) → uv sync
//  2. pixi.toml or pixi.lock → pixi install
//  3. environment.yml → conda env update
//  4. .envrc → direnv allow && eval "$(direnv export bash)"
func DetectSetupCommand(workingDir string) string {
	// uv: pyproject.toml + (uv.lock or .venv/)
	if fileExists(filepath.Join(workingDir, "pyproject.toml")) &&
		(fileExists(filepath.Join(workingDir, "uv.lock")) || dirExists(filepath.Join(workingDir, ".venv"))) {
		return "uv sync"
	}

	// pixi: pixi.toml or pixi.lock
	if fileExists(filepath.Join(workingDir, "pixi.toml")) || fileExists(filepath.Join(workingDir, "pixi.lock")) {
		return "pixi install"
	}

	// conda: environment.yml
	if fileExists(filepath.Join(workingDir, "environment.yml")) {
		return "conda env update"
	}

	// direnv: .envrc
	if fileExists(filepath.Join(workingDir, ".envrc")) {
		return direnvSetupCommand
	}

	return ""
}

// RunSetupCommand runs a detected environment setup command synchronously.
// If timeout > 0, the process is killed after that duration (exit code 124,
// matching the timeout(1) convention). A zero timeout uses DefaultSetupTimeout.
// The process group ID is written to the job's PGID file so the kill poller
// can reach the process during setup.
// Returns ExitInfo and error. On success, ExitInfo.ExitCode is 0 and error is nil.
func RunSetupCommand(setupCmd string, jobID int64, workingDir string, envVars []string, paths JobPaths, timeout time.Duration) (ExitInfo, error) {
	if timeout == 0 {
		timeout = inventory.DefaultSetupTimeout
	}
	slog.Debug("running setup command", "component", "runner", "job_id", jobID, "cmd", setupCmd, "timeout", timeout)
	proc, err := StartProcess(setupCmd, workingDir, envVars, paths.Log)
	if err != nil {
		slog.Warn("setup start failed", "component", "runner", "job_id", jobID, "error", err)
		ei := ExitInfo{ExitCode: 1}
		WriteStatusFile(paths, ei)
		return ei, fmt.Errorf("setup command: %w", err)
	}

	// Write PGID file so the kill poller can reach the setup process.
	proc.WritePIDFiles(paths)

	// Enforce setup timeout.
	var timedOut atomic.Bool
	if timeout > 0 {
		timer := time.AfterFunc(timeout, func() {
			timedOut.Store(true)
			slog.Warn("setup timeout reached, sending SIGTERM", "component", "runner", "job_id", jobID, "timeout", timeout, "pgid", proc.PGID)
			syscall.Kill(-proc.PGID, syscall.SIGTERM)
			time.AfterFunc(10*time.Second, func() {
				syscall.Kill(-proc.PGID, syscall.SIGKILL)
			})
		})
		defer timer.Stop()
	}

	if waitErr := proc.Cmd.Wait(); waitErr != nil {
		if timedOut.Load() {
			ei := ExitInfo{ExitCode: 124}
			slog.Warn("setup command timed out", "component", "runner", "job_id", jobID, "timeout", timeout, "cmd", setupCmd)
			WriteStatusFile(paths, ei)
			return ei, fmt.Errorf("setup command timed out after %s", timeout)
		}
		ei := ExtractExitInfo(waitErr)
		slog.Warn("setup command failed", "component", "runner", "job_id", jobID, "exit_code", ei.ExitCode, "cmd", setupCmd)
		WriteStatusFile(paths, ei)
		return ei, fmt.Errorf("setup command failed: %w", waitErr)
	}
	return ExitInfo{}, nil
}

// ResolveDirenvEnv evaluates .envrc and returns the exported environment.
// The returned slice is a full environment suitable for passing to exec.Cmd.Env.
func ResolveDirenvEnv(workingDir string, envVars []string, logPath string) ([]string, ExitInfo, error) {
	baseEnv := mergeEnvVars(os.Environ(), envVars)

	cmd := exec.Command("bash", "-lc", "direnv allow >/dev/null && direnv exec . env -0")
	cmd.Dir = workingDir
	cmd.Env = baseEnv

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if stderr.Len() > 0 {
		appendSetupLog(logPath, stderr.Bytes())
	}
	if err != nil {
		return nil, ExtractExitInfo(err), fmt.Errorf("resolve direnv environment: %w", err)
	}

	resolved := parseNullSeparatedEnv(stdout.Bytes())
	if len(resolved) == 0 {
		return nil, ExitInfo{ExitCode: 1}, fmt.Errorf("resolve direnv environment: empty environment")
	}

	return resolved, ExitInfo{}, nil
}

func parseNullSeparatedEnv(data []byte) []string {
	parts := bytes.Split(data, []byte{0})
	env := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		env = append(env, string(part))
	}
	return env
}

func appendSetupLog(path string, data []byte) {
	if path == "" || len(data) == 0 {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(data)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
