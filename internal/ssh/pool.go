package ssh

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/osteele/weft/internal/config"
)

// ErrPoolBusy is returned by TryExecute when all pool slots for a host are occupied.
var ErrPoolBusy = errors.New("all pool slots busy")

// ErrCommandTimeout is returned when a command exceeds its timeout.
var ErrCommandTimeout = errors.New("command timed out")

var (
	defaultPoolSize     = 4
	defaultMaxParallel  = 8
	defaultConnTimeout  = 10 // seconds, passed to ssh -o ConnectTimeout
	defaultReadyTimeout = 15 * time.Second
	defaultPool         *SessionPool
	poolOnce            sync.Once
)

func init() {
	// Apply config file settings first
	cfg, _ := config.Load()
	if cfg != nil {
		if n := cfg.SSH.PoolSize; n > 0 {
			defaultPoolSize = n
		}
		if n := cfg.SSH.MaxParallel; n > 0 {
			defaultMaxParallel = n
		}
		if n := cfg.SSH.ConnectTimeout; n > 0 {
			defaultConnTimeout = n
			defaultReadyTimeout = time.Duration(n+5) * time.Second
		}
	}

	// Environment variables override config file
	if s := os.Getenv("WEFT_SSH_POOL_SIZE"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			defaultPoolSize = n
		}
	}
	if s := os.Getenv("WEFT_SSH_MAX_PARALLEL"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			defaultMaxParallel = n
		}
	}
	if s := os.Getenv("WEFT_SSH_CONNECT_TIMEOUT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			defaultConnTimeout = n
			defaultReadyTimeout = time.Duration(n+5) * time.Second
		}
	}
}

// SetMinConnectTimeout raises the SSH connect timeout to at least the given
// duration. It has no effect if the current timeout is already larger. This
// must be called before the first SSH operation (the pool is created lazily).
func SetMinConnectTimeout(d time.Duration) {
	secs := int(d.Seconds())
	if secs > defaultConnTimeout {
		defaultConnTimeout = secs
		defaultReadyTimeout = time.Duration(secs+5) * time.Second
	}
}

// getDefaultPool returns the global session pool, creating it lazily.
func getDefaultPool() *SessionPool {
	poolOnce.Do(func() {
		defaultPool = NewSessionPool(defaultPoolSize, defaultMaxParallel)
	})
	return defaultPool
}

// ClosePool shuts down the global SSH session pool.
func ClosePool() {
	if defaultPool != nil {
		defaultPool.Close()
	}
}

// SessionPool manages persistent SSH sessions across hosts via long-lived
// ssh+bash subprocesses, one shell command per round-trip. Do NOT switch to
// OpenSSH ControlMaster connection multiplexing — it has been tried for weft
// and found unsuitable. Reproduce the original failure on current hosts and
// capture the outcome here before changing the transport.
type SessionPool struct {
	mu        sync.Mutex
	hosts     map[string]*hostPool
	size      int
	closed    bool
	globalSem chan struct{} // limits total concurrent SSH operations across all hosts
}

type hostPool struct {
	host     string
	sessions []*Session
	idle     chan *Session
	sem      chan struct{} // semaphore limiting concurrency
	mu       sync.Mutex
	size     int
}

// Session is a persistent ssh bash process.
type Session struct {
	host      string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	stderr    *bufio.Reader
	alive     bool
	closeOnce sync.Once
}

// commandError represents a non-zero exit code from a remote command.
// The session is still healthy and can be reused.
type commandError struct {
	exitCode int
}

func (e *commandError) Error() string {
	return fmt.Sprintf("exit status %d", e.exitCode)
}

// NewSessionPool creates a pool with the given per-host size and global max parallel limit.
func NewSessionPool(size int, maxParallel int) *SessionPool {
	return &SessionPool{
		hosts:     make(map[string]*hostPool),
		size:      size,
		globalSem: make(chan struct{}, maxParallel),
	}
}

func (p *SessionPool) getHostPool(host string) *hostPool {
	p.mu.Lock()
	defer p.mu.Unlock()
	hp, ok := p.hosts[host]
	if !ok {
		hp = &hostPool{
			host: host,
			idle: make(chan *Session, p.size),
			sem:  make(chan struct{}, p.size),
			size: p.size,
		}
		p.hosts[host] = hp
	}
	return hp
}

// Execute runs a command on a remote host using a pooled session.
func (p *SessionPool) Execute(host, command string, timeout time.Duration) (string, string, error) {
	if p.closed {
		return "", "", fmt.Errorf("SSH connection unavailable")
	}

	// Acquire global semaphore first
	if timeout > 0 {
		select {
		case p.globalSem <- struct{}{}:
		case <-time.After(timeout):
			return "", "", fmt.Errorf("SSH connections busy, try again")
		}
	} else {
		p.globalSem <- struct{}{}
	}
	defer func() { <-p.globalSem }()

	hp := p.getHostPool(host)

	// Acquire per-host semaphore
	if timeout > 0 {
		select {
		case hp.sem <- struct{}{}:
		case <-time.After(timeout):
			return "", "", fmt.Errorf("SSH connection to %s busy, try again", host)
		}
	} else {
		hp.sem <- struct{}{}
	}
	defer func() { <-hp.sem }()

	return p.executeWithSemaphore(hp, command, timeout)
}

// TryExecute runs a command like Execute but returns ErrPoolBusy immediately
// if all semaphore slots for the host are occupied. Use this for periodic/best-effort
// callers that should skip rather than queue up.
func (p *SessionPool) TryExecute(host, command string, timeout time.Duration) (string, string, error) {
	if p.closed {
		return "", "", fmt.Errorf("SSH connection unavailable")
	}

	// Non-blocking global semaphore acquire
	select {
	case p.globalSem <- struct{}{}:
	default:
		return "", "", ErrPoolBusy
	}
	defer func() { <-p.globalSem }()

	hp := p.getHostPool(host)

	// Non-blocking per-host semaphore acquire
	select {
	case hp.sem <- struct{}{}:
	default:
		return "", "", ErrPoolBusy
	}
	defer func() { <-hp.sem }()

	return p.executeWithSemaphore(hp, command, timeout)
}

// executeWithSemaphore runs a command after the semaphore has been acquired.
func (p *SessionPool) executeWithSemaphore(hp *hostPool, command string, timeout time.Duration) (string, string, error) {
	sess, err := hp.acquire()
	if err != nil {
		return "", "", err
	}

	stdout, stderr, err := sess.execute(command, timeout)
	if err != nil {
		var cmdErr *commandError
		if errors.As(err, &cmdErr) {
			hp.release(sess)
			return stdout, stderr, err
		}
		sess.close()
		hp.discard(sess)
		return stdout, stderr, err
	}

	hp.release(sess)
	return stdout, stderr, err
}

// Close shuts down all sessions.
func (p *SessionPool) Close() {
	p.mu.Lock()
	p.closed = true
	hosts := make([]*hostPool, 0, len(p.hosts))
	for _, hp := range p.hosts {
		hosts = append(hosts, hp)
	}
	p.mu.Unlock()

	for _, hp := range hosts {
		hp.closeAll()
	}
}

func (hp *hostPool) acquire() (*Session, error) {
	// Try to get an idle session
	select {
	case sess := <-hp.idle:
		if sess.alive && sess.ping() {
			return sess, nil
		}
		sess.close()
		// fall through to create new
	default:
	}

	// Create a new session
	return hp.newSession()
}

func (hp *hostPool) release(sess *Session) {
	if !sess.alive {
		return
	}
	select {
	case hp.idle <- sess:
	default:
		// idle channel full, close this session
		sess.close()
	}
}

func (hp *hostPool) discard(_ *Session) {
	// Nothing to track; the session was already closed by the caller.
}

const sessionReadyMarker = "---RJ-READY---"

func (hp *hostPool) newSession() (*Session, error) {
	// Use "echo READY; exec bash -s" so the remote shell signals
	// readiness before replacing itself with bash. This avoids writing
	// to stdin before the SSH channel is established (SSH drops data
	// sent to stdin before the remote shell is ready).
	remoteCmd := fmt.Sprintf("echo '%s'; exec bash -s", sessionReadyMarker)
	sshArgs := BatchModeArgs(
		time.Duration(defaultConnTimeout)*time.Second,
		"ServerAliveInterval=15",
		"ServerAliveCountMax=3",
	)
	sshArgs = append(sshArgs, hp.host, remoteCmd)
	cmd := execCommand("ssh", sshArgs...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("SSH to %s: %w", hp.host, err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("SSH to %s: %w", hp.host, err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("SSH to %s: %w", hp.host, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("SSH to %s failed: %w", hp.host, err)
	}

	stdoutReader := bufio.NewReader(stdoutPipe)

	if debugSSH {
		slog.Debug("new SSH session, waiting for ready", "component", "ssh", "host", hp.host, "pid", cmd.Process.Pid)
	}

	// Wait for the ready marker from the remote shell.
	readyCh := make(chan error, 1)
	go func() {
		for {
			line, readErr := stdoutReader.ReadString('\n')
			if readErr != nil {
				readyCh <- fmt.Errorf("SSH connection to %s failed: %w", hp.host, readErr)
				return
			}
			if strings.TrimSpace(line) == sessionReadyMarker {
				readyCh <- nil
				return
			}
		}
	}()

	select {
	case err := <-readyCh:
		if err != nil {
			killAndWait(cmd)
			// Capture stderr for a more informative error message
			stderrBytes, _ := io.ReadAll(stderrPipe)
			stderrMsg := strings.TrimSpace(string(stderrBytes))
			if stderrMsg != "" {
				if IsConnectionError(stderrMsg) {
					return nil, fmt.Errorf("%s is offline", hp.host)
				}
				return nil, fmt.Errorf("ssh %s: %s", hp.host, stderrMsg)
			}
			return nil, err
		}
	case <-time.After(defaultReadyTimeout):
		killAndWait(cmd)
		return nil, fmt.Errorf("SSH connection to %s timed out", hp.host)
	}

	sess := &Session{
		host:   hp.host,
		cmd:    cmd,
		stdin:  stdinPipe,
		stdout: stdoutReader,
		stderr: bufio.NewReader(stderrPipe),
		alive:  true,
	}

	if debugSSH {
		slog.Debug("SSH session ready", "component", "ssh", "host", hp.host, "pid", cmd.Process.Pid)
	}

	return sess, nil
}

func killAndWait(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func (hp *hostPool) closeAll() {
	hp.mu.Lock()
	defer hp.mu.Unlock()

	// Drain idle channel
	for {
		select {
		case sess := <-hp.idle:
			sess.close()
		default:
			return
		}
	}
}

func (s *Session) ping() bool {
	return s.pingWithTimeout(2 * time.Second)
}

func (s *Session) pingWithTimeout(timeout time.Duration) bool {
	if !s.alive {
		return false
	}
	id := uuid.New().String()[:8]
	marker := fmt.Sprintf("---PING-%s---", id)

	_, err := fmt.Fprintf(s.stdin, "echo '%s'\n", marker)
	if err != nil {
		s.alive = false
		return false
	}

	// Read with timeout
	done := make(chan bool, 1)
	go func() {
		for {
			line, err := s.stdout.ReadString('\n')
			if err != nil {
				done <- false
				return
			}
			if strings.TrimSpace(line) == marker {
				done <- true
				return
			}
		}
	}()

	select {
	case ok := <-done:
		return ok
	case <-time.After(timeout):
		s.alive = false
		// Kill the SSH process so the leaked goroutine unblocks on the
		// closed stdout pipe and can exit.
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		return false
	}
}

func (s *Session) execute(command string, timeout time.Duration) (string, string, error) {
	if !s.alive {
		return "", "", fmt.Errorf("session is dead")
	}

	id := uuid.New().String()[:8]
	startMarker := fmt.Sprintf("---START-%s---", id)
	ecodePrefix := fmt.Sprintf("---ECODE-%s-", id)
	endMarker := fmt.Sprintf("---END-%s---", id)
	doneMarker := fmt.Sprintf("---DONE-%s---", id)

	// Write the wrapped command
	script := fmt.Sprintf(
		"__RJ_ERR=$(mktemp)\necho '%s'\n( %s ) 2>\"${__RJ_ERR}\"\necho '%s'\"$?---\"\necho '%s'\ncat \"${__RJ_ERR}\" >&2\nrm -f \"${__RJ_ERR}\"\necho '%s' >&2\n",
		startMarker, command, ecodePrefix, endMarker, doneMarker,
	)

	_, err := io.WriteString(s.stdin, script)
	if err != nil {
		s.alive = false
		return "", "", fmt.Errorf("write command: %w", err)
	}

	type result struct {
		stdout string
		stderr string
		code   int
		err    error
	}

	ch := make(chan result, 1)
	go func() {
		var stdoutBuf strings.Builder
		var stderrBuf strings.Builder
		exitCode := 0
		started := false

		// Read stdout until endMarker
		for {
			line, readErr := s.stdout.ReadString('\n')
			if readErr != nil {
				s.alive = false
				ch <- result{err: fmt.Errorf("read stdout: %w", readErr)}
				return
			}
			trimmed := strings.TrimRight(line, "\n")

			if !started {
				if trimmed == startMarker {
					started = true
				}
				continue
			}

			if trimmed == endMarker {
				break
			}

			if strings.HasPrefix(trimmed, ecodePrefix) {
				// Extract exit code from ---ECODE-<id>-N---
				codeStr := strings.TrimPrefix(trimmed, ecodePrefix)
				codeStr = strings.TrimSuffix(codeStr, "---")
				if n, parseErr := strconv.Atoi(codeStr); parseErr == nil {
					exitCode = n
				}
				continue
			}

			stdoutBuf.WriteString(line)
		}

		// Read stderr until doneMarker
		for {
			line, readErr := s.stderr.ReadString('\n')
			if readErr != nil {
				s.alive = false
				ch <- result{err: fmt.Errorf("read stderr: %w", readErr)}
				return
			}
			trimmed := strings.TrimRight(line, "\n")
			if trimmed == doneMarker {
				break
			}
			stderrBuf.WriteString(line)
		}

		var cmdErr error
		if exitCode != 0 {
			cmdErr = &commandError{exitCode: exitCode}
		}

		ch <- result{
			stdout: stdoutBuf.String(),
			stderr: stderrBuf.String(),
			code:   exitCode,
			err:    cmdErr,
		}
	}()

	if timeout <= 0 {
		timeout = 30 * time.Second // default max
	}

	select {
	case r := <-ch:
		return r.stdout, r.stderr, r.err
	case <-time.After(timeout):
		s.alive = false
		// Kill immediately so the reader goroutine unblocks on closed pipes.
		// s.close() would wait up to 5s for graceful exit, which is pointless
		// after a timeout.
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		s.close()
		return "", "", fmt.Errorf("command on %s after %v: %w", s.host, timeout, ErrCommandTimeout)
	}
}

func (s *Session) close() {
	s.closeOnce.Do(func() {
		s.alive = false

		// Try graceful exit
		_, _ = io.WriteString(s.stdin, "exit\n")
		_ = s.stdin.Close()

		// Wait briefly, then kill
		done := make(chan struct{})
		go func() {
			_ = s.cmd.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
		}
	})
}
