package runner

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

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
		return `direnv allow && eval "$(direnv export bash)"`
	}

	return ""
}

// RunSetupCommand runs a detected environment setup command synchronously.
// Returns ExitInfo and error. On success, ExitInfo.ExitCode is 0 and error is nil.
func RunSetupCommand(setupCmd string, jobID int64, workingDir string, envVars []string, paths JobPaths) (ExitInfo, error) {
	slog.Debug("running setup command", "component", "runner", "job_id", jobID, "cmd", setupCmd)
	proc, err := StartProcess(setupCmd, workingDir, envVars, paths.Log)
	if err != nil {
		slog.Warn("setup start failed", "component", "runner", "job_id", jobID, "error", err)
		ei := ExitInfo{ExitCode: 1}
		WriteStatusFile(paths, ei)
		return ei, fmt.Errorf("setup command: %w", err)
	}
	if waitErr := proc.Cmd.Wait(); waitErr != nil {
		ei := ExtractExitInfo(waitErr)
		slog.Warn("setup command failed", "component", "runner", "job_id", jobID, "exit_code", ei.ExitCode, "cmd", setupCmd)
		WriteStatusFile(paths, ei)
		return ei, fmt.Errorf("setup command failed: %w", waitErr)
	}
	return ExitInfo{}, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
