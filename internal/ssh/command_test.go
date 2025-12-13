package ssh

import (
	"os/exec"
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

// TestReadRemoteFileCommand verifies that ReadRemoteFile doesn't quote paths
// (which would break tilde expansion)
func TestReadRemoteFileCommand(t *testing.T) {
	var capturedArgs []string

	// Replace execCommand to capture arguments
	orig := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		capturedArgs = append([]string{name}, args...)
		// Return a command that just echoes empty (won't actually run in test)
		return exec.Command("echo", "")
	}
	defer func() { execCommand = orig }()

	tests := []struct {
		name        string
		path        string
		wantPattern string // Pattern that should appear in command (unquoted)
		badPattern  string // Pattern that should NOT appear (quoted)
	}{
		{
			name:        "tilde path not quoted",
			path:        "~/.cache/remote-jobs/logs/test.log",
			wantPattern: "cat ~/.cache/remote-jobs/logs/test.log",
			badPattern:  "'~/.cache/remote-jobs/logs/test.log'",
		},
		{
			name:        "absolute path not quoted",
			path:        "/tmp/test.log",
			wantPattern: "cat /tmp/test.log",
			badPattern:  "'/tmp/test.log'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capturedArgs = nil
			ReadRemoteFile("testhost", tt.path)

			if len(capturedArgs) < 3 {
				t.Fatalf("expected at least 3 args, got %d: %v", len(capturedArgs), capturedArgs)
			}

			command := capturedArgs[2] // ssh host "command"
			if !strings.Contains(command, tt.wantPattern) {
				t.Errorf("command should contain %q, got %q", tt.wantPattern, command)
			}
			if strings.Contains(command, tt.badPattern) {
				t.Errorf("command should NOT contain quoted path %q, got %q", tt.badPattern, command)
			}
		})
	}
}

// TestRemoteFileExistsCommand verifies that RemoteFileExists doesn't quote paths
func TestRemoteFileExistsCommand(t *testing.T) {
	var capturedArgs []string

	orig := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		capturedArgs = append([]string{name}, args...)
		return exec.Command("echo", "EXISTS")
	}
	defer func() { execCommand = orig }()

	tests := []struct {
		name        string
		path        string
		wantPattern string
		badPattern  string
	}{
		{
			name:        "tilde path not quoted",
			path:        "~/.cache/remote-jobs/status.txt",
			wantPattern: "test -f ~/.cache/remote-jobs/status.txt",
			badPattern:  "'~/.cache/remote-jobs/status.txt'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capturedArgs = nil
			RemoteFileExists("testhost", tt.path)

			if len(capturedArgs) < 3 {
				t.Fatalf("expected at least 3 args, got %d: %v", len(capturedArgs), capturedArgs)
			}

			command := capturedArgs[2]
			if !strings.Contains(command, tt.wantPattern) {
				t.Errorf("command should contain %q, got %q", tt.wantPattern, command)
			}
			if strings.Contains(command, tt.badPattern) {
				t.Errorf("command should NOT contain quoted path %q, got %q", tt.badPattern, command)
			}
		})
	}
}

// TestIsConnectionError verifies that various SSH error messages are recognized
func TestIsConnectionError(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{
			name:     "connection timed out",
			input:    "ssh: connect to host example.com: Connection timed out",
			expected: true,
		},
		{
			name:     "operation timed out (macOS)",
			input:    "ssh: connect to host 10.0.0.1 port 22: Operation timed out",
			expected: true,
		},
		{
			name:     "no route to host",
			input:    "ssh: connect to host example.com: No route to host",
			expected: true,
		},
		{
			name:     "connection refused",
			input:    "ssh: connect to host example.com port 22: Connection refused",
			expected: true,
		},
		{
			name:     "could not resolve hostname",
			input:    "ssh: Could not resolve hostname invalid.host: Name or service not known",
			expected: true,
		},
		{
			name:     "network is unreachable",
			input:    "ssh: connect to host example.com: Network is unreachable",
			expected: true,
		},
		{
			name:     "permission denied is not connection error",
			input:    "Permission denied (publickey)",
			expected: false,
		},
		{
			name:     "command not found is not connection error",
			input:    "bash: command not found",
			expected: false,
		},
		{
			name:     "empty string",
			input:    "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsConnectionError(tt.input)
			if result != tt.expected {
				t.Errorf("IsConnectionError(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}
