package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCdPrefix(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		wantDir  string
		wantRest string
	}{
		{"unquoted with &&", "cd /path/to/dir && python train.py", "/path/to/dir", "python train.py"},
		{"unquoted with semicolon", "cd /path/to/dir; python train.py", "/path/to/dir", "python train.py"},
		{"single-quoted path", "cd '/path/to my dir' && python train.py", "/path/to my dir", "python train.py"},
		{"double-quoted path", `cd "/path/to my dir" && python train.py`, "/path/to my dir", "python train.py"},
		{"no cd prefix", "python train.py", "", "python train.py"},
		{"cd only no separator", "cd /path/to/dir", "", "cd /path/to/dir"},
		{"tilde path", "cd ~/projects && make", "~/projects", "make"},
		{"leading whitespace", "  cd /foo && bar", "/foo", "bar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, rest := parseCdPrefix(tt.command)
			if dir != tt.wantDir || rest != tt.wantRest {
				t.Errorf("parseCdPrefix(%q) = (%q, %q), want (%q, %q)",
					tt.command, dir, rest, tt.wantDir, tt.wantRest)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"safe string", "hello", "hello"},
		{"empty string", "", "''"},
		{"spaces", "hello world", "'hello world'"},
		{"special chars", "foo$bar", "'foo$bar'"},
		{"inner single quotes", "it's", "'it'\"'\"'s'"},
		{"tilde", "~/path", "'~/path'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPathHasHomePrefix(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	tests := []struct {
		name string
		dir  string
		home string
		want bool
	}{
		{"exact match", home, home, true},
		{"subdirectory", filepath.Join(home, "projects"), home, true},
		{"sibling", home + "-other", home, false},
		{"unrelated", "/tmp/foo", home, false},
		{"trailing slash cleaned", home + "/", home, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pathHasHomePrefix(tt.dir, tt.home)
			if got != tt.want {
				t.Errorf("pathHasHomePrefix(%q, %q) = %v, want %v", tt.dir, tt.home, got, tt.want)
			}
		})
	}
}
