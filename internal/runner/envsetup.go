package runner

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/inventory"
)

const direnvSetupCommand = `direnv allow && eval "$(direnv export bash)"`

var uvSystemPythonPath = "/opt/conda/bin/python"

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

// ShouldSkipSetup reports whether the detected setup command should be skipped
// because the script's [tool.weft] metadata declares `isolated = true`.
// An isolated script manages its own dependencies via PEP 723 inline metadata
// and does not need a project-level `uv sync`.
func ShouldSkipSetup(setupCmd string, meta *dataloc.ScriptMeta) bool {
	if setupCmd != "uv sync" {
		return false
	}
	return meta != nil && meta.Isolated
}

// WarnIfWorkdirMissingEnv writes a warning to the job's log file when no
// setup command was detected but the working directory looks like it should
// have triggered one. Without this, a wiped or partially-staged project dir
// produces a silent ModuleNotFoundError (the script imports a package that
// uv sync would have installed, but uv sync was never run because the lock
// file or pyproject.toml is missing). Logging the underlying state turns
// that into a self-explanatory failure.
//
// Cases that warrant a warning:
//   - working directory does not exist or is empty (likely wiped between jobs)
//   - pyproject.toml present but neither uv.lock nor .venv (project state
//     inconsistent — uv sync detection logic gives up silently here)
func WarnIfWorkdirMissingEnv(workingDir string, jobID int64, logPath string) {
	if workingDir == "" {
		return
	}
	info, err := os.Stat(workingDir)
	if err != nil || !info.IsDir() {
		slog.Warn("working directory missing; running command without setup",
			"component", "runner", "job_id", jobID, "workdir", workingDir, "error", err)
		appendSetupLog(logPath, []byte(fmt.Sprintf(
			"weft: working directory %q is missing; running command without env setup. Expect import errors if the job depends on a project env.\n",
			workingDir,
		)))
		return
	}
	entries, err := os.ReadDir(workingDir)
	if err == nil && len(entries) == 0 {
		slog.Warn("working directory empty; running command without setup",
			"component", "runner", "job_id", jobID, "workdir", workingDir)
		appendSetupLog(logPath, []byte(fmt.Sprintf(
			"weft: working directory %q is empty; running command without env setup. Expect import errors if the job depends on a project env.\n",
			workingDir,
		)))
		return
	}
	if fileExists(filepath.Join(workingDir, "pyproject.toml")) &&
		!fileExists(filepath.Join(workingDir, "uv.lock")) &&
		!dirExists(filepath.Join(workingDir, ".venv")) {
		slog.Warn("pyproject.toml present without uv.lock or .venv; uv sync will not run",
			"component", "runner", "job_id", jobID, "workdir", workingDir)
		appendSetupLog(logPath, []byte(
			"weft: pyproject.toml is present but neither uv.lock nor .venv exists; uv sync will not run. If this is unexpected, the project's lockfile may be missing from the working directory.\n",
		))
	}
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
	preparedEnv := envVars
	preparedCmd := setupCmd
	if setupCmd == "uv sync" {
		var prepErr error
		preparedCmd, preparedEnv, prepErr = prepareUVSyncEnvironment(workingDir, setupCmd, envVars)
		if prepErr != nil {
			slog.Warn("uv sync preflight failed; continuing with normal uv sync",
				"component", "runner", "job_id", jobID, "error", prepErr)
			appendSetupLog(paths.Log, []byte("weft: uv sync preflight failed; continuing with normal uv sync: "+prepErr.Error()+"\n"))
			preparedCmd = setupCmd
			preparedEnv = envVars
		}
	}
	appendSetupLog(paths.Log, []byte(fmt.Sprintf("weft: setup command: %s\n", preparedCmd)))
	slog.Debug("running setup command", "component", "runner", "job_id", jobID, "cmd", preparedCmd, "timeout", timeout)
	proc, err := StartProcess(preparedCmd, workingDir, preparedEnv, paths.Log)
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
			KillProcessGroupWithGrace(proc.PGID, 10*time.Second, paths, "")
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

func prepareUVSyncEnvironment(workingDir, setupCmd string, envVars []string) (string, []string, error) {
	useSystem, err := shouldUseSystemTorchPackages(workingDir)
	if err != nil {
		return setupCmd, envVars, err
	}
	if !useSystem {
		return setupCmd, envVars, nil
	}
	if err := ensureSystemSitePackagesVenv(workingDir); err != nil {
		return setupCmd, envVars, err
	}
	skip := dataloc.ImageProvidedTorchPackages(filepath.Join(workingDir, "uv.lock"))
	return setupCmd + dataloc.UVNoInstallPackageFlags(skip), mergeEnvVars(envVars, []string{"UV_PYTHON=" + uvSystemPythonPath}), nil
}

// uvRunEnvAdditions returns env vars that suppress uv's implicit sync on every
// `uv run` invocation reachable from the job. Returned when the project is a
// torch project using the system-provided pytorch environment (i.e. setup was
// `uv sync --no-install-package torch ...` against an image-managed venv); the
// implicit sync would otherwise refetch the CUDA wheels that were intentionally
// skipped at setup time.
//
// UV_NO_SYNC is set in the process env rather than as a `--no-sync` command
// flag so it covers compound shell commands, loop bodies, justfile recipes,
// and nested scripts — anywhere uv may be invoked, not just the top-level
// command string.
func uvRunEnvAdditions(workingDir, setupCmd string) ([]string, error) {
	if setupCmd != "uv sync" {
		return nil, nil
	}
	useSystem, err := shouldUseSystemTorchPackages(workingDir)
	if err != nil {
		return nil, err
	}
	if !useSystem {
		return nil, nil
	}
	return []string{"UV_NO_SYNC=1"}, nil
}

// applyUVRunEnvAdditions calls uvRunEnvAdditions and merges the result into
// envVars. Preflight errors are logged but non-fatal; the job continues with
// the unmodified env.
func applyUVRunEnvAdditions(workingDir, setupCmd string, envVars []string, jobID int64, logPath string) []string {
	uvEnv, err := uvRunEnvAdditions(workingDir, setupCmd)
	if err != nil {
		slog.Warn("uv run env-vars preflight failed; continuing without UV_NO_SYNC",
			"component", "runner", "job_id", jobID, "error", err)
		appendSetupLog(logPath, []byte("weft: uv run env-vars preflight failed; continuing without UV_NO_SYNC: "+err.Error()+"\n"))
		return envVars
	}
	if len(uvEnv) == 0 {
		return envVars
	}
	appendSetupLog(logPath, []byte("weft: UV_NO_SYNC=1 set; uv run will skip implicit sync\n"))
	return mergeEnvVars(envVars, uvEnv)
}

func shouldUseSystemTorchPackages(workingDir string) (bool, error) {
	if setupPythonPath := strings.TrimSpace(uvSystemPythonPath); setupPythonPath == "" {
		return false, nil
	}
	if _, err := os.Stat(uvSystemPythonPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", uvSystemPythonPath, err)
	}
	torchProject, err := pyprojectDependsOnTorch(filepath.Join(workingDir, "pyproject.toml"))
	if err != nil {
		return false, err
	}
	return torchProject, nil
}

func pyprojectDependsOnTorch(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read pyproject: %w", err)
	}
	lower := strings.ToLower(string(data))
	return strings.Contains(lower, "\"torch") || strings.Contains(lower, "'torch"), nil
}

func ensureSystemSitePackagesVenv(workingDir string) error {
	venvDir := filepath.Join(workingDir, ".venv")
	if dirExists(venvDir) {
		return nil
	}
	cmd := exec.Command(uvSystemPythonPath, "-m", "venv", "--system-site-packages", ".venv")
	cmd.Dir = workingDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create .venv with system site packages: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
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
