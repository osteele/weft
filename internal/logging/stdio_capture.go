package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// StdioCapture redirects file descriptors 1 (stdout) and 2 (stderr) to a
// log file (or /dev/null if no log path is provided), so writes from any
// source — fmt.Println, log.*, third-party libraries, even cgo — cannot
// corrupt a TUI rendered to the controlling terminal.
//
// TTYStdout exposes the original terminal stdout (backed by a dup'd fd).
// Pass it to tea.WithOutput so the TUI renders to the real terminal while
// everything else is diverted into the sink file.
//
// The fds are dup2'd at a regular file rather than a pipe: a pipe's
// kernel buffer (64 KB on macOS) deadlocks the writer if a drain
// goroutine ever falls behind, whereas the kernel's file-write path
// never blocks on backpressure.
type StdioCapture struct {
	// TTYStdout is a *os.File that writes to the original terminal stdout.
	// Use this with tea.WithOutput when constructing a tea.Program.
	TTYStdout *os.File

	logPath      string
	sink         io.Closer
	restoreOnce  sync.Once
	origStdoutFd int
	origStderrFd int
}

// StartStdioCapture installs the redirection. If logPath is empty (or the
// log file cannot be opened), captured bytes go to /dev/null instead.
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

	sinkFile, sinkPath, openErr := openSinkFile(logPath)
	if openErr != nil {
		_ = syscall.Close(origStdoutFd)
		_ = syscall.Close(origStderrFd)
		return nil, fmt.Errorf("open sink: %w", openErr)
	}

	if err := syscall.Dup2(int(sinkFile.Fd()), int(os.Stdout.Fd())); err != nil {
		_ = sinkFile.Close()
		_ = syscall.Close(origStdoutFd)
		_ = syscall.Close(origStderrFd)
		return nil, fmt.Errorf("dup2 stdout: %w", err)
	}
	if err := syscall.Dup2(int(sinkFile.Fd()), int(os.Stderr.Fd())); err != nil {
		_ = syscall.Dup2(origStdoutFd, int(os.Stdout.Fd()))
		_ = sinkFile.Close()
		_ = syscall.Close(origStdoutFd)
		_ = syscall.Close(origStderrFd)
		return nil, fmt.Errorf("dup2 stderr: %w", err)
	}

	return &StdioCapture{
		TTYStdout:    os.NewFile(uintptr(origStdoutFd), "tty-stdout"),
		logPath:      sinkPath,
		sink:         sinkFile,
		origStdoutFd: origStdoutFd,
		origStderrFd: origStderrFd,
	}, nil
}

// openSinkFile opens logPath for truncating writes, falling back to
// /dev/null if logPath is empty or unwritable. The returned path is what
// Restore reports to the user; "" means /dev/null was used.
func openSinkFile(logPath string) (*os.File, string, error) {
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err == nil {
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
				return f, logPath, nil
			}
		}
	}
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, "", err
	}
	return f, "", nil
}

// Restore rewires the original terminal fds back into place. After Restore,
// os.Stdout / os.Stderr write to the terminal again. If the sink file grew
// during the session, a one-line warning is printed pointing at the log
// file so leaks remain visible and fixable.
func (c *StdioCapture) Restore() {
	c.restoreOnce.Do(func() {
		_ = syscall.Dup2(c.origStdoutFd, int(os.Stdout.Fd()))
		_ = syscall.Dup2(c.origStderrFd, int(os.Stderr.Fd()))
		var capturedBytes int64
		if c.logPath != "" {
			if info, err := os.Stat(c.logPath); err == nil {
				capturedBytes = info.Size()
			}
		}
		if c.sink != nil {
			_ = c.sink.Close()
		}
		// Don't Close() TTYStdout — tea.Program may still hold a reference
		// briefly after Run() returns. The dup'd fd leaks until process exit,
		// which is fine for short-lived TUI sessions. origStderrFd was never
		// wrapped in *os.File, so close it directly.
		_ = syscall.Close(c.origStderrFd)
		if capturedBytes > 0 {
			fmt.Fprintf(os.Stderr, "weft: captured %d bytes of stdout/stderr during TUI; see %s\n", capturedBytes, c.logPath)
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
