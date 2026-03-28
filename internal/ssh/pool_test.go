package ssh

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// newLocalSession creates a session using local bash (no SSH needed).
func newLocalSession() (*Session, error) {
	cmd := exec.Command("bash", "-s")
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Session{
		host:   "local",
		cmd:    cmd,
		stdin:  stdinPipe,
		stdout: bufio.NewReader(stdoutPipe),
		stderr: bufio.NewReader(stderrPipe),
		alive:  true,
	}, nil
}

func TestSessionExecuteSimple(t *testing.T) {
	sess, err := newLocalSession()
	if err != nil {
		t.Fatalf("newLocalSession: %v", err)
	}
	defer sess.close()

	stdout, stderr, err := sess.execute("echo hello", 5*time.Second)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := strings.TrimSpace(stdout); got != "hello" {
		t.Errorf("stdout = %q, want %q", got, "hello")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestSessionExecuteExitCode(t *testing.T) {
	sess, err := newLocalSession()
	if err != nil {
		t.Fatalf("newLocalSession: %v", err)
	}
	defer sess.close()

	_, _, err = sess.execute("exit 42", 5*time.Second)
	if err == nil {
		t.Fatal("expected error for exit 42")
	}
	if !strings.Contains(err.Error(), "exit status 42") {
		t.Errorf("error = %q, want 'exit status 42'", err)
	}
}

func TestSessionExecuteStderr(t *testing.T) {
	sess, err := newLocalSession()
	if err != nil {
		t.Fatalf("newLocalSession: %v", err)
	}
	defer sess.close()

	stdout, stderr, err := sess.execute("echo out; echo err >&2", 5*time.Second)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := strings.TrimSpace(stdout); got != "out" {
		t.Errorf("stdout = %q, want %q", got, "out")
	}
	if got := strings.TrimSpace(stderr); got != "err" {
		t.Errorf("stderr = %q, want %q", got, "err")
	}
}

func TestSessionMultipleCommands(t *testing.T) {
	sess, err := newLocalSession()
	if err != nil {
		t.Fatalf("newLocalSession: %v", err)
	}
	defer sess.close()

	for i := range 5 {
		stdout, _, err := sess.execute(fmt.Sprintf("echo %d", i), 5*time.Second)
		if err != nil {
			t.Fatalf("execute #%d: %v", i, err)
		}
		if got := strings.TrimSpace(stdout); got != fmt.Sprintf("%d", i) {
			t.Errorf("execute #%d: stdout = %q, want %q", i, got, fmt.Sprintf("%d", i))
		}
	}
}

func TestSessionPing(t *testing.T) {
	sess, err := newLocalSession()
	if err != nil {
		t.Fatalf("newLocalSession: %v", err)
	}
	defer sess.close()

	if !sess.ping() {
		t.Error("ping should succeed on live session")
	}
}

func TestSessionPingDeadSession(t *testing.T) {
	sess, err := newLocalSession()
	if err != nil {
		t.Fatalf("newLocalSession: %v", err)
	}

	// Kill the process
	sess.cmd.Process.Kill()
	sess.cmd.Wait()
	// Give pipes a moment to close
	time.Sleep(50 * time.Millisecond)
	sess.alive = true // force ping to try

	if sess.ping() {
		t.Error("ping should fail on dead session")
	}
}

func TestSessionClose(t *testing.T) {
	sess, err := newLocalSession()
	if err != nil {
		t.Fatalf("newLocalSession: %v", err)
	}

	sess.close()
	if sess.alive {
		t.Error("session should not be alive after close")
	}
}

func TestSessionExecuteTimeout(t *testing.T) {
	sess, err := newLocalSession()
	if err != nil {
		t.Fatalf("newLocalSession: %v", err)
	}

	_, _, err = sess.execute("sleep 60", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, ErrCommandTimeout) {
		t.Errorf("error = %q, want ErrCommandTimeout", err)
	}
}

func TestKillAndWaitReapsProcess(t *testing.T) {
	cmd := exec.Command("bash", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	killAndWait(cmd)

	if cmd.ProcessState == nil {
		t.Fatal("expected process state after killAndWait")
	}
}

func TestSessionExecuteMultilineOutput(t *testing.T) {
	sess, err := newLocalSession()
	if err != nil {
		t.Fatalf("newLocalSession: %v", err)
	}
	defer sess.close()

	stdout, _, err := sess.execute("echo -e 'line1\\nline2\\nline3'", 5*time.Second)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Errorf("got %d lines, want 3: %v", len(lines), lines)
	}
}

func TestSessionPoolNew(t *testing.T) {
	pool := NewSessionPool(2, 8)
	defer pool.Close()

	if pool.size != 2 {
		t.Errorf("pool size = %d, want 2", pool.size)
	}
}

// Verify WriteString to a closed stdin returns error
func TestSessionWriteAfterClose(t *testing.T) {
	sess, err := newLocalSession()
	if err != nil {
		t.Fatalf("newLocalSession: %v", err)
	}
	sess.close()

	_, writeErr := io.WriteString(sess.stdin, "echo test\n")
	if writeErr == nil {
		t.Error("expected error writing to closed session")
	}
}
