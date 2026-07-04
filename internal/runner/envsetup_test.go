package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
)

func TestDetectSetupCommand(t *testing.T) {
	tests := []struct {
		name  string
		setup func(dir string)
		want  string
	}{
		{
			name:  "empty directory",
			setup: func(dir string) {},
			want:  "",
		},
		{
			name: "uv: pyproject.toml + .venv",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0644)
				os.MkdirAll(filepath.Join(dir, ".venv"), 0755)
			},
			want: "uv sync",
		},
		{
			name: "uv: pyproject.toml + uv.lock (no .venv)",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0644)
				os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(""), 0644)
			},
			want: "uv sync",
		},
		{
			name: "pyproject.toml alone is not detected",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0644)
			},
			want: "",
		},
		{
			name: "pixi.toml",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pixi.toml"), []byte(""), 0644)
			},
			want: "pixi install",
		},
		{
			name: "pixi.lock only",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pixi.lock"), []byte(""), 0644)
			},
			want: "pixi install",
		},
		{
			name: "environment.yml",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "environment.yml"), []byte(""), 0644)
			},
			want: "conda env update",
		},
		{
			name: ".envrc",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, ".envrc"), []byte(""), 0644)
			},
			want: `direnv allow && eval "$(direnv export bash)"`,
		},
		{
			name: "uv takes priority over pixi",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0644)
				os.MkdirAll(filepath.Join(dir, ".venv"), 0755)
				os.WriteFile(filepath.Join(dir, "pixi.toml"), []byte(""), 0644)
			},
			want: "uv sync",
		},
		{
			name: "pixi takes priority over conda",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "pixi.toml"), []byte(""), 0644)
				os.WriteFile(filepath.Join(dir, "environment.yml"), []byte(""), 0644)
			},
			want: "pixi install",
		},
		{
			name: "conda takes priority over direnv",
			setup: func(dir string) {
				os.WriteFile(filepath.Join(dir, "environment.yml"), []byte(""), 0644)
				os.WriteFile(filepath.Join(dir, ".envrc"), []byte(""), 0644)
			},
			want: "conda env update",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(dir)
			got := DetectSetupCommand(dir)
			if got != tt.want {
				t.Errorf("DetectSetupCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunSetupCommand_Timeout(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	paths := NewJobPaths(logDir, 999)

	start := time.Now()
	ei, err := RunSetupCommand("sleep 300", 999, dir, nil, paths, 1*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from timed-out setup command")
	}
	if ei.ExitCode != 124 {
		t.Fatalf("exit code = %d, want 124 (timeout convention)", ei.ExitCode)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("setup should have been killed quickly, took %s", elapsed)
	}
}

func TestRunSetupCommand_WritesPGIDFile(t *testing.T) {
	dir := t.TempDir()
	logDir := t.TempDir()
	paths := NewJobPaths(logDir, 888)

	// Run a fast command that succeeds
	ei, err := RunSetupCommand("true", 888, dir, nil, paths, 10*time.Second)
	if err != nil {
		t.Fatalf("RunSetupCommand() error = %v", err)
	}
	if ei.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", ei.ExitCode)
	}

	// Verify PGID file was written
	pgidPath := filepath.Join(logDir, "888.pgid")
	if _, err := os.Stat(pgidPath); os.IsNotExist(err) {
		t.Fatal("PGID file should exist after setup command runs")
	}
}

func TestPyprojectDependsOnTorch(t *testing.T) {
	dir := t.TempDir()
	pyproject := filepath.Join(dir, "pyproject.toml")
	if err := os.WriteFile(pyproject, []byte("[project]\ndependencies = [\"torch>=2.6\"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}

	got, err := pyprojectDependsOnTorch(pyproject)
	if err != nil {
		t.Fatalf("pyprojectDependsOnTorch() error = %v", err)
	}
	if !got {
		t.Fatal("pyprojectDependsOnTorch() = false, want true")
	}
}

func TestPyprojectDependsOnTorch_FalseWhenMissing(t *testing.T) {
	dir := t.TempDir()
	pyproject := filepath.Join(dir, "pyproject.toml")
	if err := os.WriteFile(pyproject, []byte("[project]\ndependencies = [\"numpy>=1.0\"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}

	got, err := pyprojectDependsOnTorch(pyproject)
	if err != nil {
		t.Fatalf("pyprojectDependsOnTorch() error = %v", err)
	}
	if got {
		t.Fatal("pyprojectDependsOnTorch() = true, want false")
	}
}

func TestShouldSkipSetup(t *testing.T) {
	isolated := &dataloc.ScriptMeta{Isolated: true}
	notIsolated := &dataloc.ScriptMeta{GPU: "nvidia"}

	tests := []struct {
		name     string
		setupCmd string
		meta     *dataloc.ScriptMeta
		want     bool
	}{
		{"uv sync + isolated", "uv sync", isolated, true},
		{"uv sync + not isolated", "uv sync", notIsolated, false},
		{"uv sync + nil meta", "uv sync", nil, false},
		{"pixi install + isolated", "pixi install", isolated, false},
		{"empty setup + isolated", "", isolated, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldSkipSetup(tt.setupCmd, tt.meta)
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPrepareUVSyncEnvironment_UsesSystemPythonForTorchProject(t *testing.T) {
	skipSlowInShort(t)
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\ndependencies = [\"torch>=2.6\"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
	prev := uvSystemPythonPath
	uvSystemPythonPath = pythonPath
	t.Cleanup(func() { uvSystemPythonPath = prev })

	gotCmd, gotEnv, gotReason, err := prepareUVSyncEnvironment(dir, "uv sync", []string{"A=1"}, "")
	if err != nil {
		t.Fatalf("prepareUVSyncEnvironment() error = %v", err)
	}
	if gotReason != "" {
		t.Fatalf("expected empty fallback reason, got: %q", gotReason)
	}
	if _, err := os.Stat(filepath.Join(dir, ".venv")); err != nil {
		t.Fatalf("expected .venv to be created: %v", err)
	}
	joined := strings.Join(gotEnv, "\n")
	if !strings.Contains(joined, "UV_PYTHON="+pythonPath) {
		t.Fatalf("expected UV_PYTHON override in env, got: %v", gotEnv)
	}
	if !strings.Contains(gotCmd, "--no-install-package torch") {
		t.Fatalf("expected torch skip flag in command, got: %q", gotCmd)
	}
}

func TestPrepareUVSyncEnvironment_ExcludesCUDARuntimePackages(t *testing.T) {
	skipSlowInShort(t)
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\ndependencies = [\"torch>=2.6\"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(`
[[package]]
name = "torch"
version = "2.6.0"

[[package]]
name = "nvidia-cusparse-cu12"
version = "12.3.1.170"
`), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}
	prev := uvSystemPythonPath
	uvSystemPythonPath = pythonPath
	t.Cleanup(func() { uvSystemPythonPath = prev })

	gotCmd, _, _, err := prepareUVSyncEnvironment(dir, "uv sync", nil, "")
	if err != nil {
		t.Fatalf("prepareUVSyncEnvironment() error = %v", err)
	}
	for _, want := range []string{"--no-install-package torch", "--no-install-package nvidia-cusparse-cu12"} {
		if !strings.Contains(gotCmd, want) {
			t.Fatalf("expected %q in command, got: %q", want, gotCmd)
		}
	}
}

func TestUVRunEnvAdditions_SetsNoSyncForTorchProject(t *testing.T) {
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\ndependencies = [\"torch>=2.6\"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
	prev := uvSystemPythonPath
	uvSystemPythonPath = pythonPath
	t.Cleanup(func() { uvSystemPythonPath = prev })

	got, err := uvRunEnvAdditions(dir, "uv sync")
	if err != nil {
		t.Fatalf("uvRunEnvAdditions() error = %v", err)
	}
	want := []string{"UV_NO_SYNC=1"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestUVRunEnvAdditions_EmptyWhenNoSystemTorch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\ndependencies = [\"numpy>=1.0\"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}

	got, err := uvRunEnvAdditions(dir, "uv sync")
	if err != nil {
		t.Fatalf("uvRunEnvAdditions() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestUVRunEnvAdditions_EmptyWhenSetupNotUVSync(t *testing.T) {
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\ndependencies = [\"torch>=2.6\"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
	prev := uvSystemPythonPath
	uvSystemPythonPath = pythonPath
	t.Cleanup(func() { uvSystemPythonPath = prev })

	// Non-uv setup (e.g. pixi, conda) leaves UV_NO_SYNC unset because the
	// project venv is not managed by uv.
	got, err := uvRunEnvAdditions(dir, "pixi install")
	if err != nil {
		t.Fatalf("uvRunEnvAdditions() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

// writeTorchPyproject writes a minimal torch-using pyproject.toml.
func writeTorchPyproject(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\ndependencies = [\"torch>=2.6\"]\n"), 0o644); err != nil {
		t.Fatalf("write pyproject: %v", err)
	}
}

// setSystemPython points uvSystemPythonPath at the given interpreter for
// the duration of the test. Returns the resolved interpreter path.
func setSystemPython(t *testing.T) string {
	t.Helper()
	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	prev := uvSystemPythonPath
	uvSystemPythonPath = pythonPath
	t.Cleanup(func() { uvSystemPythonPath = prev })
	return pythonPath
}

func TestEnsureSystemSitePackagesVenv_RebuildsWhenSystemSitePackagesFalse(t *testing.T) {
	skipSlowInShort(t)
	pythonPath := setSystemPython(t)
	dir := t.TempDir()
	venvDir := filepath.Join(dir, ".venv")
	if err := os.MkdirAll(venvDir, 0o755); err != nil {
		t.Fatalf("mkdir .venv: %v", err)
	}
	// Pre-existing venv with the wrong flag — simulates the broken state
	// the user's `uv sync` leaves behind on a .python-version mismatch.
	cfg := "home = " + filepath.Dir(pythonPath) + "\ninclude-system-site-packages = false\nversion = 3.11.12\n"
	if err := os.WriteFile(filepath.Join(venvDir, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write pyvenv.cfg: %v", err)
	}
	// Sentinel file inside the venv: should be gone after rebuild.
	sentinel := filepath.Join(venvDir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	if err := ensureSystemSitePackagesVenv(dir, ""); err != nil {
		t.Fatalf("ensureSystemSitePackagesVenv: %v", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("expected sentinel to be removed during rebuild, got err=%v", err)
	}
	got, err := os.ReadFile(filepath.Join(venvDir, "pyvenv.cfg"))
	if err != nil {
		t.Fatalf("read rebuilt pyvenv.cfg: %v", err)
	}
	if !strings.Contains(string(got), "include-system-site-packages = true") {
		t.Fatalf("rebuilt venv missing include-system-site-packages=true, got:\n%s", got)
	}
}

func TestEnsureSystemSitePackagesVenv_RebuildsWhenHomeMismatches(t *testing.T) {
	skipSlowInShort(t)
	pythonPath := setSystemPython(t)
	dir := t.TempDir()
	venvDir := filepath.Join(dir, ".venv")
	if err := os.MkdirAll(venvDir, 0o755); err != nil {
		t.Fatalf("mkdir .venv: %v", err)
	}
	// system-site-packages is true, but home points elsewhere (simulates
	// uv recreating the venv on a different Python interpreter).
	cfg := "home = /some/other/python/bin\ninclude-system-site-packages = true\nversion = 3.13.13\n"
	if err := os.WriteFile(filepath.Join(venvDir, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write pyvenv.cfg: %v", err)
	}

	if err := ensureSystemSitePackagesVenv(dir, ""); err != nil {
		t.Fatalf("ensureSystemSitePackagesVenv: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(venvDir, "pyvenv.cfg"))
	if err != nil {
		t.Fatalf("read rebuilt pyvenv.cfg: %v", err)
	}
	expectedHome := "home = " + filepath.Dir(pythonPath)
	if !strings.Contains(string(got), expectedHome) {
		t.Fatalf("rebuilt venv home does not match %q, got:\n%s", expectedHome, got)
	}
}

func TestVenvMatchesSystemContract_AcceptsAdditionalAcceptedHome(t *testing.T) {
	// Regression: when uvSystemPythonPath is a symlink, `python -m venv`
	// writes the resolved interpreter directory into pyvenv.cfg `home`,
	// which need not equal filepath.Dir(uvSystemPythonPath). The contract
	// check must accept the probed canonical home as an additional match.
	dir := t.TempDir()
	venvDir := filepath.Join(dir, ".venv")
	if err := os.MkdirAll(venvDir, 0o755); err != nil {
		t.Fatalf("mkdir .venv: %v", err)
	}
	cfg := "home = /opt/conda/envs/base/bin\ninclude-system-site-packages = true\nversion = 3.11.12\n"
	if err := os.WriteFile(filepath.Join(venvDir, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write pyvenv.cfg: %v", err)
	}
	// Lexical home /opt/conda/bin would NOT match; the probed home
	// /opt/conda/envs/base/bin must rescue the comparison.
	if !venvMatchesSystemContract(venvDir, "/opt/conda/bin/python", "/opt/conda/envs/base/bin") {
		t.Fatal("expected probed home to satisfy the contract")
	}
	if venvMatchesSystemContract(venvDir, "/opt/conda/bin/python") {
		t.Fatal("expected lexical-only check to fail without probed home")
	}
}

func TestEnsureSystemSitePackagesVenv_KeepsValidVenv(t *testing.T) {
	pythonPath := setSystemPython(t)
	dir := t.TempDir()
	venvDir := filepath.Join(dir, ".venv")
	if err := os.MkdirAll(venvDir, 0o755); err != nil {
		t.Fatalf("mkdir .venv: %v", err)
	}
	cfg := "home = " + filepath.Dir(pythonPath) + "\ninclude-system-site-packages = true\nversion = 3.11.12\n"
	if err := os.WriteFile(filepath.Join(venvDir, "pyvenv.cfg"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write pyvenv.cfg: %v", err)
	}
	// Sentinel file inside the venv: should survive an unchanged call.
	sentinel := filepath.Join(venvDir, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	if err := ensureSystemSitePackagesVenv(dir, ""); err != nil {
		t.Fatalf("ensureSystemSitePackagesVenv: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("expected sentinel to survive (contract was honored), got err=%v", err)
	}
}

func TestShouldUseSystemTorchPackages_DeclinesOnPythonVersionMismatch(t *testing.T) {
	setSystemPython(t)
	dir := t.TempDir()
	writeTorchPyproject(t, dir)
	// 99.99 will never match any real Python install.
	if err := os.WriteFile(filepath.Join(dir, ".python-version"), []byte("99.99\n"), 0o644); err != nil {
		t.Fatalf("write .python-version: %v", err)
	}

	use, reason, err := shouldUseSystemTorchPackages(dir)
	if err != nil {
		t.Fatalf("shouldUseSystemTorchPackages: %v", err)
	}
	if use {
		t.Fatalf("expected shortcut to be declined, got use=true")
	}
	if !strings.Contains(reason, "99.99") {
		t.Fatalf("expected reason to cite mismatched version, got: %q", reason)
	}

	// Verify prepareUVSyncEnvironment surfaces the reason and returns a
	// bare `uv sync` with no UV_PYTHON override.
	cmd, env, gotReason, err := prepareUVSyncEnvironment(dir, "uv sync", []string{"A=1"}, "")
	if err != nil {
		t.Fatalf("prepareUVSyncEnvironment: %v", err)
	}
	if cmd != "uv sync" {
		t.Fatalf("expected bare 'uv sync', got: %q", cmd)
	}
	for _, v := range env {
		if strings.HasPrefix(v, "UV_PYTHON=") {
			t.Fatalf("expected no UV_PYTHON override on fallback, got: %v", env)
		}
	}
	if gotReason != reason {
		t.Fatalf("reason mismatch: prepare returned %q, predicate returned %q", gotReason, reason)
	}
}

func TestShouldUseSystemTorchPackages_AcceptsMatchingPythonVersion(t *testing.T) {
	setSystemPython(t)
	sys, err := systemPythonMajorMinor()
	if err != nil {
		t.Skipf("cannot probe system python: %v", err)
	}
	dir := t.TempDir()
	writeTorchPyproject(t, dir)
	if err := os.WriteFile(filepath.Join(dir, ".python-version"), []byte(sys+"\n"), 0o644); err != nil {
		t.Fatalf("write .python-version: %v", err)
	}

	use, reason, err := shouldUseSystemTorchPackages(dir)
	if err != nil {
		t.Fatalf("shouldUseSystemTorchPackages: %v", err)
	}
	if !use {
		t.Fatalf("expected shortcut to be used, got use=false (reason=%q)", reason)
	}
	if reason != "" {
		t.Fatalf("expected empty reason on match, got: %q", reason)
	}
}

func TestPythonVersionRequest_HandlesVariousFormats(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"bare major.minor", "3.13\n", "3.13"},
		{"bare major.minor.patch", "3.13.1\n", "3.13"},
		{"cpython implementation", "cpython@3.13\n", "3.13"},
		{"cpython dash form", "cpython-3.13\n", "3.13"},
		{"pypy implementation preserved", "pypy@3.10\n", "pypy:3.10"},
		{"pypy dash form preserved", "pypy-3.10\n", "pypy:3.10"},
		// Regression: dotted prefixes are versions, not implementations.
		// Earlier code used strconv.Atoi to decide impl-vs-version and
		// misread "3.13" as an impl name, suppressing the version match.
		{"version dash suffix accepted", "3.13-rc1\n", "3.13"},
		{"version dash patch suffix accepted", "3.13.5-foo\n", "3.13"},
		{"prerelease rc accepted", "3.13rc1\n", "3.13"},
		{"prerelease alpha accepted", "3.13.0a3\n", "3.13"},
		{"dev suffix accepted", "3.13.dev0\n", "3.13"},
		{"leading blank lines", "\n\n3.12\n", "3.12"},
		{"comment lines skipped", "# pin\n3.11\n", "3.11"},
		{"junk first line skipped", "latest\n3.13\n", "3.13"},
		{"junk only file", "latest\nbroken\n", ""},
		{"only one component", "3\n", ""},
		{"empty file", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.content != "" || tt.name == "empty file" {
				if err := os.WriteFile(filepath.Join(dir, ".python-version"), []byte(tt.content), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if got := pythonVersionRequest(dir); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}

	// Missing file → "" (no error).
	t.Run("missing file", func(t *testing.T) {
		if got := pythonVersionRequest(t.TempDir()); got != "" {
			t.Fatalf("expected empty for missing file, got %q", got)
		}
	})
}
