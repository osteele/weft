package dataloc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirectUVRunPEP723Script(t *testing.T) {
	dir := t.TempDir()
	pepScript := filepath.Join(dir, "scripts", "train.py")
	if err := os.MkdirAll(filepath.Dir(pepScript), 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(pepScript, []byte("# /// script\n# dependencies = []\n# ///\n"), 0o644); err != nil {
		t.Fatalf("write PEP 723 script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.py"), []byte("print('plain')\n"), 0o644); err != nil {
		t.Fatalf("write plain script: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.py"), []byte("CONFIG = {}\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"direct", "uv run scripts/train.py --mode full", "scripts/train.py"},
		{"leading environment", "HF_HOME=/cache uv run scripts/train.py", "scripts/train.py"},
		{"uv path", "/usr/local/bin/uv run scripts/train.py", "scripts/train.py"},
		{"uv options", "uv run --python '>=3.11,<3.13' --with numpy scripts/train.py", "scripts/train.py"},
		{"option with Python file value", "uv run --env-file config.py scripts/train.py", "scripts/train.py"},
		{"explicit separator", "uv run -- scripts/train.py", "scripts/train.py"},
		{"python bypasses script environment", "uv run python scripts/train.py", ""},
		{"plain Python", "python scripts/train.py", ""},
		{"missing metadata", "uv run plain.py", ""},
		{"compound command", "python prep.py && uv run scripts/train.py", ""},
		{"different executable", "uv run bash scripts/train.py", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DirectUVRunPEP723Script(dir, tt.command); got != tt.want {
				t.Fatalf("DirectUVRunPEP723Script() = %q, want %q", got, tt.want)
			}
		})
	}
}
