package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
)

// StdioCapture redirects file descriptors 1 (stdout) and 2 (stderr) to a
// background drain that writes captured bytes to a log file. This catches
// writes from any source — fmt.Println, log.*, third-party libraries, even
// cgo — so they cannot corrupt a TUI rendered to the controlling terminal.
//
// TTYStdout exposes the original terminal stdout (backed by a dup'd fd).
// Pass it to tea.WithOutput so the TUI renders to the real terminal while
// everything else is diverted.
type StdioCapture struct {
	// TTYStdout is a *os.File that writes to the original terminal stdout.
	// Use this with tea.WithOutput when constructing a tea.Program.
	TTYStdout *os.File

	logPath       string
	pipeR         *os.File
	pipeW         *os.File
	sink          io.WriteCloser
	bytesCaptured atomic.Int64
	drainDone     sync.WaitGroup
	restoreOnce   sync.Once
	origStdoutFd  int
	origStderrFd  int
}

// StartStdioCapture installs the redirection and starts the drain goroutine.
// If logPath is empty, captured bytes are discarded.
func StartStdioCapture(logPath string) (*StdioCapture, error) {
	origStdoutFd, err := syscall.Dup(int(os.Stdout.Fd()))
	if err != nil {
		return nil, fmt.Errorf("dup stdout: %w", err)
	}
	origStderrFd, err := syscall.Dup(int(os.Stderr.Fd()))
	if err != nil {
		_ = syscall.Close(origStdoutFd)
		return nil, fmt.Errorf("dup stderr: %w", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		_ = syscall.Close(origStdoutFd)
		_ = syscall.Close(origStderrFd)
		return nil, fmt.Errorf("pipe: %w", err)
	}

	if err := syscall.Dup2(int(w.Fd()), int(os.Stdout.Fd())); err != nil {
		_ = r.Close()
		_ = w.Close()
		_ = syscall.Close(origStdoutFd)
		_ = syscall.Close(origStderrFd)
		return nil, fmt.Errorf("dup2 stdout: %w", err)
	}
	if err := syscall.Dup2(int(w.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = syscall.Dup2(origStdoutFd, int(os.Stdout.Fd()))
		_ = r.Close()
		_ = w.Close()
		_ = syscall.Close(origStdoutFd)
		_ = syscall.Close(origStderrFd)
		return nil, fmt.Errorf("dup2 stderr: %w", err)
	}

	c := &StdioCapture{
		TTYStdout:    os.NewFile(uintptr(origStdoutFd), "tty-stdout"),
		logPath:      logPath,
		pipeR:        r,
		pipeW:        w,
		origStdoutFd: origStdoutFd,
		origStderrFd: origStderrFd,
	}

	if logPath != "" {
		if mkErr := os.MkdirAll(filepath.Dir(logPath), 0o755); mkErr == nil {
			f, openErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if openErr == nil {
				c.sink = f
			}
		}
	}

	c.drainDone.Add(1)
	go c.drain()

	return c, nil
}

func (c *StdioCapture) drain() {
	defer c.drainDone.Done()
	buf := make([]byte, 4096)
	for {
		n, readErr := c.pipeR.Read(buf)
		if n > 0 {
			c.bytesCaptured.Add(int64(n))
			if c.sink != nil {
				_, _ = c.sink.Write(buf[:n])
			}
		}
		if readErr != nil {
			return
		}
	}
}

// Restore rewires the original terminal fds back into place. After Restore,
// os.Stdout / os.Stderr write to the terminal again. If any bytes were
// captured during the session, a one-line warning is printed pointing at
// the log file so leaks remain visible and fixable.
func (c *StdioCapture) Restore() {
	c.restoreOnce.Do(func() {
		_ = syscall.Dup2(c.origStdoutFd, int(os.Stdout.Fd()))
		_ = syscall.Dup2(c.origStderrFd, int(os.Stderr.Fd()))
		_ = c.pipeW.Close()
		c.drainDone.Wait()
		_ = c.pipeR.Close()
		if c.sink != nil {
			_ = c.sink.Close()
		}
		// Don't Close() TTYStdout — tea.Program may still hold a reference
		// briefly after Run() returns. The dup'd fd leaks until process exit,
		// which is fine for short-lived TUI sessions. origStderrFd was never
		// wrapped in *os.File, so close it directly.
		_ = syscall.Close(c.origStderrFd)
		if n := c.bytesCaptured.Load(); n > 0 && c.logPath != "" {
			fmt.Fprintf(os.Stderr, "weft: captured %d bytes of stdout/stderr during TUI; see %s\n", n, c.logPath)
		}
	})
}

// DefaultStdioCaptureLog returns the default path for TUI stdio capture logs.
func DefaultStdioCaptureLog() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "weft", "tui-stdout.log")
}
