package runner

import (
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
//  1. pyproject.toml + .venv/ → uv sync
//  2. pixi.toml or pixi.lock → pixi install
//  3. environment.yml → conda env update
//  4. .envrc → direnv allow && eval "$(direnv export bash)"
func DetectSetupCommand(workingDir string) string {
	// uv: pyproject.toml + .venv/
	if fileExists(filepath.Join(workingDir, "pyproject.toml")) && dirExists(filepath.Join(workingDir, ".venv")) {
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

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
