package runner

import (
	"os"
	"path/filepath"
	"testing"
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
