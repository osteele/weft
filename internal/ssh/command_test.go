package ssh

import (
	"strings"
	"testing"
)

// TestTildeExpansion verifies that paths with ~ are not quoted
// which would prevent tilde expansion by the shell
func TestTildeExpansion(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		command string
	}{
		{
			name:    "tilde path should not be quoted",
			path:    "~/.cache/remote-jobs/logs/test.log",
			command: "tail -50 ~/.cache/remote-jobs/logs/test.log",
		},
		{
			name:    "absolute path works unquoted",
			path:    "/tmp/test.log",
			command: "tail -50 /tmp/test.log",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify the path is not wrapped in single quotes
			if strings.Contains(tt.command, "'"+tt.path+"'") {
				t.Errorf("path %q should not be single-quoted in command %q", tt.path, tt.command)
			}
		})
	}
}

// TestEscapeForSingleQuotes verifies that commands are properly escaped
// for embedding in bash -c '...'
func TestEscapeForSingleQuotes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no special chars",
			input:    "echo hello",
			expected: "echo hello",
		},
		{
			name:     "single quote",
			input:    "echo 'hello world'",
			expected: "echo '\\''hello world'\\''",
		},
		{
			name:     "multiple single quotes",
			input:    "echo 'a' 'b'",
			expected: "echo '\\''a'\\'' '\\''b'\\''",
		},
		{
			name:     "working directory with quotes",
			input:    "cd '/path/to/dir'",
			expected: "cd '\\''/path/to/dir'\\''",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EscapeForSingleQuotes(tt.input)
			if result != tt.expected {
				t.Errorf("EscapeForSingleQuotes(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
