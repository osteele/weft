package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
