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

	gotCmd, gotEnv, err := prepareUVSyncEnvironment(dir, "uv sync", []string{"A=1"})
	if err != nil {
		t.Fatalf("prepareUVSyncEnvironment() error = %v", err)
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

	gotCmd, _, err := prepareUVSyncEnvironment(dir, "uv sync", nil)
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
