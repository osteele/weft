package ssh

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	defaultPoolSize = 4
	defaultPool     *SessionPool
	poolOnce        sync.Once
)

func init() {
	if s := os.Getenv("REMOTE_JOBS_SSH_POOL_SIZE"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			defaultPoolSize = n
		}
	}
}

// getDefaultPool returns the global session pool, creating it lazily.
func getDefaultPool() *SessionPool {
	poolOnce.Do(func() {
		defaultPool = NewSessionPool(defaultPoolSize)
	})
	return defaultPool
}

// ClosePool shuts down the global SSH session pool.
func ClosePool() {
	if defaultPool != nil {
		defaultPool.Close()
	}
}

// SessionPool manages persistent SSH sessions across hosts.
type SessionPool struct {
	mu     sync.Mutex
	hosts  map[string]*hostPool
	size   int
	closed bool
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

// NewSessionPool creates a pool with the given per-host size.
func NewSessionPool(size int) *SessionPool {
	return &SessionPool{
		hosts: make(map[string]*hostPool),
		size:  size,
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
		return "", "", fmt.Errorf("pool is closed")
	}

	hp := p.getHostPool(host)

	// Acquire semaphore (with optional timeout)
	if timeout > 0 {
		select {
		case hp.sem <- struct{}{}:
		case <-time.After(timeout):
			return "", "", fmt.Errorf("ssh pool: timeout waiting for session to %s", host)
		}
	} else {
		hp.sem <- struct{}{}
	}
	defer func() { <-hp.sem }()

	// Get or create a session
	sess, err := hp.acquire()
	if err != nil {
		return "", "", err
	}

	stdout, stderr, err := sess.execute(command, timeout)
	if err != nil {
		// Command errors (non-zero exit) are normal; session is still healthy
		var cmdErr *commandError
		if errors.As(err, &cmdErr) {
			hp.release(sess)
			return stdout, stderr, err
		}
		// Session-level error: discard the session
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

func (hp *hostPool) newSession() (*Session, error) {
	cmd := exec.Command("ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		hp.host, "bash -s")

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh pool: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh pool: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh pool: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh pool: start: %w", err)
	}

	sess := &Session{
		host:   hp.host,
		cmd:    cmd,
		stdin:  stdinPipe,
		stdout: bufio.NewReader(stdoutPipe),
		stderr: bufio.NewReader(stderrPipe),
		alive:  true,
	}

	if debugSSH {
		log.Printf("[SSH Pool] new session to %s (pid=%d)", hp.host, cmd.Process.Pid)
	}

	return sess, nil
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
	case <-time.After(2 * time.Second):
		s.alive = false
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
		timeout = 5 * time.Minute // default max
	}

	select {
	case r := <-ch:
		return r.stdout, r.stderr, r.err
	case <-time.After(timeout):
		s.alive = false
		s.close()
		return "", "", fmt.Errorf("ssh command timed out after %v", timeout)
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
