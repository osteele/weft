package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// ErrSetupStalled marks a setup command killed because it stopped producing
// output, as distinct from one that ran to its wall-clock deadline. A stalled
// transfer and a genuinely slow one are different facts and warrant different
// retries.
var ErrSetupStalled = errors.New("setup command stalled")

// RunSetupCommand runs a detected environment setup command synchronously.
// If timeout > 0, the process is killed after that duration (exit code 124,
// matching the timeout(1) convention). A zero timeout uses DefaultSetupTimeout.
// The process group ID is written to the job's PGID file so the kill poller
// can reach the process during setup.
// Returns ExitInfo and error. On success, ExitInfo.ExitCode is 0 and error is nil.
func RunSetupCommand(setupCmd string, jobID int64, workingDir string, envVars []string, paths JobPaths, timeout time.Duration) (ExitInfo, error) {
	return RunSetupCommandWithStallTimeout(setupCmd, jobID, workingDir, envVars, paths, timeout, 0)
}

// RunSetupCommandWithStallTimeout additionally kills the command when its log
// has not grown for stallTimeout, returning ErrSetupStalled. A wall-clock
// timeout alone bounds a wedged command at the whole budget, so a hung
// download pays the maximum every time and the retry loop multiplies it; a
// stall bound charges stallTimeout instead. Pass 0 to disable.
func RunSetupCommandWithStallTimeout(setupCmd string, jobID int64, workingDir string, envVars []string, paths JobPaths, timeout, stallTimeout time.Duration) (ExitInfo, error) {
	if timeout == 0 {
		timeout = inventory.DefaultSetupTimeout
	}
	preparedEnv := envVars
	preparedCmd := setupCmd
	if setupCmd == "uv sync" {
		var prepErr error
		var fallbackReason string
		preparedCmd, preparedEnv, fallbackReason, prepErr = prepareUVSyncEnvironment(workingDir, setupCmd, envVars, paths.Log)
		if prepErr != nil {
			slog.Warn("uv sync preflight failed; continuing with normal uv sync",
				"component", "runner", "job_id", jobID, "error", prepErr)
			appendSetupLog(paths.Log, []byte("weft: uv sync preflight failed; continuing with normal uv sync: "+prepErr.Error()+"\n"))
			preparedCmd = setupCmd
			preparedEnv = envVars
		}
		if fallbackReason != "" {
			appendSetupLog(paths.Log, []byte("weft: "+fallbackReason+"\n"))
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
			KillProcessGroupWithGrace(proc.PGID, DefaultKillGrace, paths, "")
		})
		defer timer.Stop()
	}

	var stalled atomic.Bool
	if stallTimeout > 0 && paths.Log != "" {
		stopStallWatch := watchSetupStall(paths.Log, stallTimeout, func() {
			stalled.Store(true)
			slog.Warn("setup command stalled, sending SIGTERM", "component", "runner", "job_id", jobID, "stall_timeout", stallTimeout, "pgid", proc.PGID)
			appendSetupLog(paths.Log, []byte(fmt.Sprintf("weft: no setup output for %s; treating as stalled and terminating\n", stallTimeout)))
			KillProcessGroupWithGrace(proc.PGID, DefaultKillGrace, paths, "")
		})
		defer stopStallWatch()
	}

	if waitErr := proc.Cmd.Wait(); waitErr != nil {
		if stalled.Load() {
			ei := ExitInfo{ExitCode: 124}
			slog.Warn("setup command stalled", "component", "runner", "job_id", jobID, "stall_timeout", stallTimeout, "cmd", setupCmd)
			WriteStatusFile(paths, ei)
			return ei, fmt.Errorf("%w: no output for %s", ErrSetupStalled, stallTimeout)
		}
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

// prepareUVSyncEnvironment returns the prepared setup command, env
// additions, and an optional fallback reason that the caller should
// append to the job log to explain why the system-torch shortcut was
// declined. logPath is also used internally to log shortcut-applied and
// venv-rebuild markers.
func prepareUVSyncEnvironment(workingDir, setupCmd string, envVars []string, logPath string) (string, []string, string, error) {
	useSystem, fallbackReason, err := shouldUseSystemTorchPackages(workingDir)
	if err != nil {
		return setupCmd, envVars, "", err
	}
	if !useSystem {
		return setupCmd, envVars, fallbackReason, nil
	}
	if err := ensureSystemSitePackagesVenv(workingDir, logPath); err != nil {
		return setupCmd, envVars, "", err
	}
	skip := dataloc.ImageProvidedTorchPackages(filepath.Join(workingDir, "uv.lock"))
	if logPath != "" {
		appendSetupLog(logPath, []byte("weft: system-torch shortcut: UV_PYTHON="+uvSystemPythonPath+", skipping image-provided torch packages\n"))
	}
	return setupCmd + dataloc.UVNoInstallPackageFlags(skip), mergeEnvVars(envVars, []string{"UV_PYTHON=" + uvSystemPythonPath}), "", nil
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
	useSystem, _, err := shouldUseSystemTorchPackages(workingDir)
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

// shouldUseSystemTorchPackages reports whether the cloud-image
// system-torch shortcut applies. A non-empty second return is a fallback
// reason the caller should surface — set when the shortcut was a
// candidate but declined (currently: .python-version pins a major.minor
// the system Python can't satisfy).
func shouldUseSystemTorchPackages(workingDir string) (bool, string, error) {
	if setupPythonPath := strings.TrimSpace(uvSystemPythonPath); setupPythonPath == "" {
		return false, "", nil
	}
	if _, err := os.Stat(uvSystemPythonPath); err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("stat %s: %w", uvSystemPythonPath, err)
	}
	torchProject, err := pyprojectDependsOnTorch(filepath.Join(workingDir, "pyproject.toml"))
	if err != nil {
		return false, "", err
	}
	if !torchProject {
		return false, "", nil
	}
	// A project .python-version that doesn't match the system Python
	// breaks the contract on every cycle. Decline so a normal `uv sync`
	// runs.
	req := pythonVersionRequest(workingDir)
	if req == "" {
		return true, "", nil
	}
	sys, sysErr := systemPythonMajorMinor()
	if sysErr != nil {
		slog.Warn("system python version probe failed; assuming compatible with .python-version",
			"component", "runner", "python", uvSystemPythonPath, "error", sysErr)
		return true, "", nil
	}
	if req != sys {
		return false, fmt.Sprintf("system-torch shortcut declined: .python-version=%s does not match %s (%s); running full uv sync", req, sys, uvSystemPythonPath), nil
	}
	return true, "", nil
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

// pyVersionLineRE matches leading "major.minor" digits, ignoring PEP 440
// suffixes like "rc1" / "0a3" / "dev".
var pyVersionLineRE = regexp.MustCompile(`^(\d+)\.(\d+)`)

func isAlphaUnderscore(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_') {
			return false
		}
	}
	return true
}

// pythonVersionRequest reads .python-version and returns the requested
// major.minor, or "" if the file is absent or contains no parseable line.
//
// Non-CPython implementations are returned with their prefix preserved
// (e.g. "pypy:3.10") so the system-torch shortcut — which is always
// CPython from the cloud image — declines via string inequality with
// systemPythonMajorMinor's output.
func pythonVersionRequest(workingDir string) string {
	data, err := os.ReadFile(filepath.Join(workingDir, ".python-version"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		impl := ""
		if idx := strings.Index(line, "@"); idx >= 0 {
			impl = strings.ToLower(line[:idx])
			line = line[idx+1:]
		} else if i := strings.Index(line, "-"); i > 0 {
			// "cpython-3.13" / "pypy-3.10" form: prefix before the first
			// dash is the implementation only when it's a pure
			// letters/underscore identifier. Otherwise dotted version
			// forms like "3.13-rc1" or "3.13.5-foo" would be misread as
			// impl="3.13", suppressing the decline-on-mismatch path.
			prefix := line[:i]
			if prefix != "" && isAlphaUnderscore(prefix) {
				impl = strings.ToLower(prefix)
				line = line[i+1:]
			}
		}
		m := pyVersionLineRE.FindStringSubmatch(line)
		if m == nil {
			continue // junk line; try the next
		}
		ver := m[1] + "." + m[2]
		if impl != "" && impl != "cpython" {
			return impl + ":" + ver
		}
		return ver
	}
	return ""
}

// The probe runs before RunSetupCommand's setup_timeout watchdog arms,
// so it needs its own deadline to keep a hung interpreter from wedging
// setup.
const systemPythonProbeTimeout = 10 * time.Second

// -I runs Python in isolated mode (suppresses PYTHONSTARTUP, sitecustomize,
// and usercustomize) so the two output lines aren't preceded by banners.
const systemPythonProbeScript = `import sys, os
print('%d.%d' % (sys.version_info[0], sys.version_info[1]))
print(os.path.dirname(getattr(sys, '_base_executable', sys.executable)))`

// systemPythonProbe returns the interpreter's runtime major.minor and the
// directory `python -m venv` would record as `home` in pyvenv.cfg. The
// home value follows symlinks the way Python's venv module does, so
// callers can compare it against an existing pyvenv.cfg's home without a
// separate filepath.EvalSymlinks dance.
func systemPythonProbe() (version, venvHome string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), systemPythonProbeTimeout)
	defer cancel()
	out, runErr := exec.CommandContext(ctx, uvSystemPythonPath, "-I", "-c", systemPythonProbeScript).Output()
	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", "", fmt.Errorf("probe %s: timed out after %s", uvSystemPythonPath, systemPythonProbeTimeout)
		}
		return "", "", fmt.Errorf("probe %s: %w", uvSystemPythonPath, runErr)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 2 {
		return "", "", fmt.Errorf("probe %s: expected 2 lines, got %q", uvSystemPythonPath, string(out))
	}
	// Take the last two lines in case anything leaked past -I.
	version = strings.TrimSpace(lines[len(lines)-2])
	venvHome = strings.TrimSpace(lines[len(lines)-1])
	return version, venvHome, nil
}

func systemPythonMajorMinor() (string, error) {
	ver, _, err := systemPythonProbe()
	return ver, err
}

// venvMatchesSystemContract reports whether the venv at venvDir was built
// with --system-site-packages against expectedPython.
//
// `home` in pyvenv.cfg is accepted as either filepath.Dir(expectedPython)
// or any value in additionalAcceptedHomes — pass the value returned by
// systemPythonProbe to accept symlink-resolved paths the way Python's
// venv module writes them.
func venvMatchesSystemContract(venvDir, expectedPython string, additionalAcceptedHomes ...string) bool {
	cfg, err := os.ReadFile(filepath.Join(venvDir, "pyvenv.cfg"))
	if err != nil {
		return false
	}
	acceptableHomes := append([]string{filepath.Dir(expectedPython)}, additionalAcceptedHomes...)
	hasSystemSitePackages := false
	homeMatches := false
	for _, line := range strings.Split(string(cfg), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "include-system-site-packages":
			if strings.EqualFold(strings.TrimSpace(value), "true") {
				hasSystemSitePackages = true
			}
		case "home":
			got := strings.TrimSpace(value)
			for _, accepted := range acceptableHomes {
				if got == accepted {
					homeMatches = true
					break
				}
			}
		}
	}
	return hasSystemSitePackages && homeMatches
}

// ensureSystemSitePackagesVenv brings .venv into compliance with the
// system-torch contract. Wipes and rebuilds when the existing venv
// doesn't match. When logPath is non-empty, the rebuild is announced
// there so users grepping the job log can see why .venv contents were
// discarded.
func ensureSystemSitePackagesVenv(workingDir, logPath string) error {
	venvDir := filepath.Join(workingDir, ".venv")
	// Probe failure is non-fatal: fall back to the lexical check.
	var probedHomes []string
	if _, h, probeErr := systemPythonProbe(); probeErr == nil && h != "" {
		probedHomes = []string{h}
	}
	if venvMatchesSystemContract(venvDir, uvSystemPythonPath, probedHomes...) {
		return nil
	}
	venvExists := false
	if info, statErr := os.Lstat(venvDir); statErr == nil {
		venvExists = true
		if logPath != "" {
			kind := "directory"
			if !info.IsDir() {
				kind = "non-directory"
			}
			appendSetupLog(logPath, []byte(fmt.Sprintf("weft: rebuilding .venv: existing %s does not match the system-torch contract (system-site-packages + home=%s)\n", kind, filepath.Dir(uvSystemPythonPath))))
		}
	}
	if venvExists {
		if err := os.RemoveAll(venvDir); err != nil {
			return fmt.Errorf("remove stale .venv: %w", err)
		}
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

var resolveDirenvEnv = ResolveDirenvEnv

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

// watchSetupStall calls onStall once when logPath stops growing for
// stallTimeout, and returns a function that stops the watch. Polling the
// log's size is enough to tell "working slowly" from "wedged" without
// parsing content. A log that cannot be stat'd is treated as not-yet-growing
// rather than stalled: an unreadable path is unknown evidence, and killing a
// live process on it would be worse than waiting for the wall-clock timeout.
func watchSetupStall(logPath string, stallTimeout time.Duration, onStall func()) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(watchdogTickPeriod(stallTimeout))
		defer ticker.Stop()
		lastSize := int64(-1)
		if info, err := os.Stat(logPath); err == nil {
			lastSize = info.Size()
		}
		lastChange := time.Now()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				info, err := os.Stat(logPath)
				if err != nil {
					continue
				}
				if size := info.Size(); size != lastSize {
					lastSize = size
					lastChange = now
					continue
				}
				if now.Sub(lastChange) >= stallTimeout {
					onStall()
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}
