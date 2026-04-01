//go:build integration
// +build integration

package ssh

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPoolIntegrationSSH(t *testing.T) {
	host := os.Getenv("SSH_TEST_HOST")
	if host == "" {
		t.Skip("SSH_TEST_HOST not set")
	}

	pool := NewSessionPool(2, 8)
	defer pool.Close()

	// Test 1: simple echo
	t.Run("simple echo", func(t *testing.T) {
		stdout, stderr, err := pool.Execute(host, "echo hello", 10*time.Second)
		if err != nil {
			t.Fatalf("err=%v stderr=%q", err, stderr)
		}
		if got := strings.TrimSpace(stdout); got != "hello" {
			t.Errorf("stdout=%q, want %q", got, "hello")
		}
	})

	// Test 2: multi-line
	t.Run("multi-line", func(t *testing.T) {
		stdout, _, err := pool.Execute(host, `echo "ARCH:$(uname -sm)"; echo "CPUS:$(nproc 2>/dev/null || echo -)"`, 10*time.Second)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(stdout, "ARCH:") {
			t.Errorf("stdout=%q, missing ARCH:", stdout)
		}
		t.Logf("output: %s", stdout)
	})

	// Test 3: stderr
	t.Run("stderr", func(t *testing.T) {
		stdout, stderr, err := pool.Execute(host, `echo out; echo err >&2`, 10*time.Second)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if got := strings.TrimSpace(stdout); got != "out" {
			t.Errorf("stdout=%q, want %q", got, "out")
		}
		if got := strings.TrimSpace(stderr); got != "err" {
			t.Errorf("stderr=%q, want %q", got, "err")
		}
	})

	// Test 4: non-zero exit (session should be reused)
	t.Run("non-zero exit", func(t *testing.T) {
		_, _, err := pool.Execute(host, `false`, 10*time.Second)
		if err == nil {
			t.Fatal("expected error for false")
		}
		t.Logf("error (expected): %v", err)

		// Session should still work after non-zero exit
		stdout, _, err := pool.Execute(host, "echo after_error", 10*time.Second)
		if err != nil {
			t.Fatalf("session broken after non-zero exit: %v", err)
		}
		if got := strings.TrimSpace(stdout); got != "after_error" {
			t.Errorf("stdout=%q, want %q", got, "after_error")
		}
	})

	// Test 5: host info style command with separator
	t.Run("host info style", func(t *testing.T) {
		cmd := fmt.Sprintf(`echo "ARCH:$(uname -sm)"; echo "OS:$(uname -r)"; echo "CPUS:$(nproc 2>/dev/null || echo -)"; echo "%s"; echo "extra_output"`, "---RJ-SECTION---")
		stdout, _, err := pool.Execute(host, cmd, 10*time.Second)
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if !strings.Contains(stdout, "ARCH:") {
			t.Errorf("missing ARCH in: %q", stdout)
		}
		if !strings.Contains(stdout, "---RJ-SECTION---") {
			t.Errorf("missing separator in: %q", stdout)
		}
		t.Logf("output:\n%s", stdout)
	})
}
