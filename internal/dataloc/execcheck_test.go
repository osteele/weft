package dataloc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckBareScriptExecutable(t *testing.T) {
	const (
		nonExec = 0o644
		exec    = 0o755
	)

	// writeFile creates file rel under dir with the given mode and returns dir.
	type fileSpec struct {
		rel  string
		mode os.FileMode
	}

	tests := []struct {
		name        string
		files       []fileSpec
		pyproject   bool
		emptyDir    bool // pass localDir="" instead of the temp dir
		command     string
		wantErr     bool
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:        "non-exec py with pyproject suggests uv run",
			files:       []fileSpec{{"scripts/run.py", nonExec}},
			pyproject:   true,
			command:     "scripts/run.py",
			wantErr:     true,
			wantContain: []string{"uv run scripts/run.py", "chmod +x scripts/run.py"},
			wantAbsent:  []string{"python scripts/run.py"},
		},
		{
			name:        "non-exec py without pyproject suggests python",
			files:       []fileSpec{{"scripts/run.py", nonExec}},
			command:     "scripts/run.py",
			wantErr:     true,
			wantContain: []string{"python scripts/run.py", "chmod +x scripts/run.py"},
			wantAbsent:  []string{"uv run"},
		},
		{
			name:    "executable py passes",
			files:   []fileSpec{{"scripts/run.py", exec}},
			command: "scripts/run.py",
			wantErr: false,
		},
		{
			name:    "interpreter prefix python is ignored",
			files:   []fileSpec{{"scripts/run.py", nonExec}},
			command: "python scripts/run.py",
			wantErr: false,
		},
		{
			name:    "interpreter prefix uv run is ignored",
			files:   []fileSpec{{"scripts/run.py", nonExec}},
			command: "uv run scripts/run.py",
			wantErr: false,
		},
		{
			name:        "non-exec shell script suggests only chmod",
			files:       []fileSpec{{"run.sh", nonExec}},
			command:     "run.sh",
			wantErr:     true,
			wantContain: []string{"chmod +x run.sh"},
			wantAbsent:  []string{"uv run", "python run.sh"},
		},
		{
			name:     "empty localDir is a no-op",
			files:    []fileSpec{{"scripts/run.py", nonExec}},
			emptyDir: true,
			command:  "scripts/run.py",
			wantErr:  false,
		},
		{
			name:        "leading env assignment is skipped",
			files:       []fileSpec{{"scripts/run.py", nonExec}},
			command:     "FOO=bar scripts/run.py",
			wantErr:     true,
			wantContain: []string{"chmod +x scripts/run.py"},
		},
		{
			name:    "missing file is not blocked",
			files:   nil,
			command: "scripts/missing.py",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				abs := filepath.Join(dir, f.rel)
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(abs, []byte("#!/usr/bin/env python\n"), f.mode); err != nil {
					t.Fatalf("write %s: %v", f.rel, err)
				}
				// WriteFile honors umask; force the intended mode.
				if err := os.Chmod(abs, f.mode); err != nil {
					t.Fatalf("chmod %s: %v", f.rel, err)
				}
			}
			if tt.pyproject {
				if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
					t.Fatalf("write pyproject: %v", err)
				}
			}

			localDir := dir
			if tt.emptyDir {
				localDir = ""
			}

			err := CheckBareScriptExecutable(localDir, tt.command)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if err != nil {
				msg := err.Error()
				for _, want := range tt.wantContain {
					if !strings.Contains(msg, want) {
						t.Errorf("error missing %q\ngot: %s", want, msg)
					}
				}
				for _, absent := range tt.wantAbsent {
					if strings.Contains(msg, absent) {
						t.Errorf("error unexpectedly contains %q\ngot: %s", absent, msg)
					}
				}
			}
		})
	}
}
