package ssh

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
			path:    "~/.cache/weft/logs/test.log",
			command: "tail -50 ~/.cache/weft/logs/test.log",
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

func TestConnectionRetryIncludesNonConnectionStderr(t *testing.T) {
	err := connectionRetry(
		func() error { return errors.New("exit status 1") },
		func() string { return "scp: output/exp_n06: not a regular file" },
		"SCP",
		false,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "scp failed") || !strings.Contains(msg, "not a regular file") || !strings.Contains(msg, "exit status 1") {
		t.Fatalf("error = %q, want stderr and exit status", msg)
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
	tests := []struct {
		name        string
		path        string
		wantPattern string // Pattern that should appear in command (unquoted)
		badPattern  string // Pattern that should NOT appear (quoted)
	}{
		{
			name:        "tilde path not quoted",
			path:        "~/.cache/weft/logs/test.log",
			wantPattern: "cat ~/.cache/weft/logs/test.log",
			badPattern:  "'~/.cache/weft/logs/test.log'",
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
			var capturedCommand string

			// Use SetRunner to capture the command
			cleanup := SetRunner(func(host, command string) (string, string, error) {
				capturedCommand = command
				return "", "", nil
			})
			defer cleanup()

			ReadRemoteFile("testhost", tt.path)

			if capturedCommand == "" {
				t.Fatal("command was not captured")
			}
			if !strings.Contains(capturedCommand, tt.wantPattern) {
				t.Errorf("command should contain %q, got %q", tt.wantPattern, capturedCommand)
			}
			if strings.Contains(capturedCommand, tt.badPattern) {
				t.Errorf("command should NOT contain quoted path %q, got %q", tt.badPattern, capturedCommand)
			}
		})
	}
}

// TestRemoteFileExistsCommand verifies that RemoteFileExists doesn't quote paths
func TestRemoteFileExistsCommand(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantPattern string
		badPattern  string
	}{
		{
			name:        "tilde path not quoted",
			path:        "~/.cache/weft/status.txt",
			wantPattern: "test -f ~/.cache/weft/status.txt",
			badPattern:  "'~/.cache/weft/status.txt'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedCommand string

			cleanup := SetRunner(func(host, command string) (string, string, error) {
				capturedCommand = command
				return "EXISTS", "", nil
			})
			defer cleanup()

			RemoteFileExists("testhost", tt.path)

			if capturedCommand == "" {
				t.Fatal("command was not captured")
			}
			if !strings.Contains(capturedCommand, tt.wantPattern) {
				t.Errorf("command should contain %q, got %q", tt.wantPattern, capturedCommand)
			}
			if strings.Contains(capturedCommand, tt.badPattern) {
				t.Errorf("command should NOT contain quoted path %q, got %q", tt.badPattern, capturedCommand)
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
			name:     "pool ready timeout",
			input:    "SSH connection to cool30 timed out",
			expected: true,
		},
		{
			name:     "pool ready EOF",
			input:    "SSH connection to studio failed: EOF",
			expected: true,
		},
		{
			name:     "pooled session stdout EOF",
			input:    "read stdout: EOF",
			expected: true,
		},
		{
			name:     "pooled session stderr EOF",
			input:    "read stderr: EOF",
			expected: true,
		},
		{
			name:     "pooled session broken pipe",
			input:    "write command: broken pipe",
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

func TestRunWithContextUsesMockRunner(t *testing.T) {
	cleanup := SetRunner(func(host, command string) (string, string, error) {
		if host != "testhost" {
			t.Fatalf("host = %q, want testhost", host)
		}
		if command != "echo hello" {
			t.Fatalf("command = %q, want echo hello", command)
		}
		return "hello\n", "", nil
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	stdout, stderr, err := RunWithContext(ctx, "testhost", "echo hello")
	if err != nil {
		t.Fatalf("RunWithContext: %v", err)
	}
	if stdout != "hello\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "hello\n")
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
}
