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
	defaultReadyTimeout = readyTimeoutForConnect(defaultConnTimeout)
	defaultPool         *SessionPool
	poolOnce            sync.Once

	// sshIdentityFile is the cloud.ssh.identity_file path, used by
	// identityArgs as a fallback for hosts with ssh_user overrides that
	// don't specify their own per-host identity.
	sshIdentityFile string
	// sshUserByHost is hosts.<name>.ssh_user overrides; empty entries
	// fall back to ssh defaults so ~/.ssh/config keeps driving auth.
	sshUserByHost map[string]string
	// sshIdentityByHost is hosts.<name>.ssh_identity_file overrides
	// (tilde-expanded). Takes precedence over the cloud fallback so a host
	// like studio can offer its own key (e.g. ~/.ssh/agent_studio_ed25519)
	// rather than the cloud key that the host would reject. Empty entries
	// fall back to sshIdentityFile, then to ssh defaults.
	sshIdentityByHost map[string]string
)

func init() {
	// Environment variables (config-file overrides are applied later via
	// Configure(), which the CLI entry point calls after config.Load).
	// Reading config here would import the user's real ~/.config/weft state
	// into every test binary that links this package — see the "agent_studio"
	// SSH key leak that motivated splitting init from Configure.
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
			defaultReadyTimeout = readyTimeoutForConnect(n)
		}
	}
}

func readyTimeoutForConnect(connectSeconds int) time.Duration {
	// ConnectTimeout only covers establishing the TCP connection. Some reachable
	// hosts can take longer to finish login shell startup and emit the pool ready
	// marker, especially after network/VPN changes or cold SSH auth paths.
	const minReadyTimeout = 45 * time.Second
	timeout := time.Duration(connectSeconds+5) * time.Second
	if timeout < minReadyTimeout {
		return minReadyTimeout
	}
	return timeout
}

// Configure applies SSH-pool and identity settings from a loaded config.
// Call this once at process startup, before any SSH operations. Env vars
// already applied in init() take precedence over the config values here for
// pool sizing; identity settings come solely from config.
func Configure(cfg *config.Config) {
	if cfg == nil {
		return
	}
	if n := cfg.SSH.PoolSize; n > 0 && os.Getenv("WEFT_SSH_POOL_SIZE") == "" {
		defaultPoolSize = n
	}
	if n := cfg.SSH.MaxParallel; n > 0 && os.Getenv("WEFT_SSH_MAX_PARALLEL") == "" {
		defaultMaxParallel = n
	}
	if n := cfg.SSH.ConnectTimeout; n > 0 && os.Getenv("WEFT_SSH_CONNECT_TIMEOUT") == "" {
		defaultConnTimeout = n
		defaultReadyTimeout = readyTimeoutForConnect(n)
	}
	sshIdentityFile = cfg.Cloud.SSH.ExpandedIdentityFile()
	sshUserByHost = make(map[string]string, len(cfg.Hosts))
	sshIdentityByHost = make(map[string]string, len(cfg.Hosts))
	for name, hc := range cfg.Hosts {
		if hc.SSHUser != "" {
			sshUserByHost[name] = hc.SSHUser
		}
		if id := strings.TrimSpace(hc.SSHIdentityFile); id != "" {
			sshIdentityByHost[name] = config.ExpandUserPath(id)
		}
	}
}

// identityArgs returns the SSH flags that pin weft's identity file when
// the host has an explicit ssh_user override. Identity precedence:
//  1. hosts.<name>.ssh_identity_file (per-host override)
//  2. cloud.ssh.identity_file (legacy cluster-wide fallback)
//  3. none — pass through to ssh defaults / ~/.ssh/config.
//
// Applied only for hosts with ssh_user set; hosts without it pass through
// to ssh defaults (which honor ~/.ssh/config). We deliberately do NOT use
// `-F /dev/null` — that would also strip HostName aliasing from the user's
// config, breaking hosts whose canonical address only resolves via an
// alias (observed: studio). IdentitiesOnly=yes + IdentityAgent=none + -i
// are sufficient to pin our key without losing alias resolution.
//
// Without the per-host override, weft used to send the cloud key to every
// host that had an ssh_user — including studio, whose `agent` account only
// accepts ~/.ssh/agent_studio_ed25519. The result was "SSH connection to
// studio failed: EOF" (after Permission denied), which propagated as a
// 644-hour-stale host_inventory cache and silent dispatch failures.
func identityArgs(host string) []string {
	if sshUserByHost[host] == "" {
		return nil
	}
	identity := sshIdentityByHost[host]
	if identity == "" {
		identity = sshIdentityFile
	}
	if identity == "" {
		return nil
	}
	return []string{
		"-o", "IdentityAgent=none",
		"-o", "IdentitiesOnly=yes",
		"-i", identity,
	}
}

// hostTarget renders the SSH target with an optional per-host user
// override. With no override the host name passes through unchanged
// (ssh resolves user via ~/.ssh/config or local username). When
// hosts.<name>.ssh_user is set in config, the target becomes
// "user@host" so weft connects as the configured service user.
func hostTarget(host string) string {
	if user := sshUserByHost[host]; user != "" {
		return user + "@" + host
	}
	return host
}

// SetMinConnectTimeout raises the SSH connect timeout to at least the given
// duration. It has no effect if the current timeout is already larger. This
// must be called before the first SSH operation (the pool is created lazily).
func SetMinConnectTimeout(d time.Duration) {
	secs := int(d.Seconds())
	if secs > defaultConnTimeout {
		defaultConnTimeout = secs
		defaultReadyTimeout = readyTimeoutForConnect(secs)
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
	deadline := timeoutDeadline(timeout)

	// Acquire global semaphore first
	if timeout > 0 {
		remaining, ok := timeoutRemaining(deadline)
		if !ok {
			return "", "", fmt.Errorf("SSH connections busy, try again")
		}
		select {
		case p.globalSem <- struct{}{}:
		case <-time.After(remaining):
			return "", "", fmt.Errorf("SSH connections busy, try again")
		}
	} else {
		p.globalSem <- struct{}{}
	}
	defer func() { <-p.globalSem }()

	hp := p.getHostPool(host)

	// Acquire per-host semaphore
	if timeout > 0 {
		remaining, ok := timeoutRemaining(deadline)
		if !ok {
			return "", "", fmt.Errorf("SSH connection to %s busy, try again", host)
		}
		select {
		case hp.sem <- struct{}{}:
		case <-time.After(remaining):
			return "", "", fmt.Errorf("SSH connection to %s busy, try again", host)
		}
	} else {
		hp.sem <- struct{}{}
	}
	defer func() { <-hp.sem }()

	return p.executeWithSemaphore(hp, command, deadline)
}

// TryExecute runs a command like Execute but returns ErrPoolBusy immediately
// if all semaphore slots for the host are occupied. Use this for periodic/best-effort
// callers that should skip rather than queue up.
func (p *SessionPool) TryExecute(host, command string, timeout time.Duration) (string, string, error) {
	if p.closed {
		return "", "", fmt.Errorf("SSH connection unavailable")
	}
	deadline := timeoutDeadline(timeout)

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

	return p.executeWithSemaphore(hp, command, deadline)
}

// executeWithSemaphore runs a command after the semaphore has been acquired.
func (p *SessionPool) executeWithSemaphore(hp *hostPool, command string, deadline time.Time) (string, string, error) {
	sess, err := hp.acquireWithDeadline(deadline)
	if err != nil {
		return "", "", err
	}

	commandTimeout, ok := timeoutRemaining(deadline)
	if !ok {
		sess.close()
		hp.discard(sess)
		return "", "", fmt.Errorf("command on %s before start: %w", sess.host, ErrCommandTimeout)
	}
	stdout, stderr, err := sess.execute(command, commandTimeout)
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

func timeoutDeadline(timeout time.Duration) time.Time {
	if timeout <= 0 {
		return time.Time{}
	}
	return time.Now().Add(timeout)
}

func timeoutRemaining(deadline time.Time) (time.Duration, bool) {
	if deadline.IsZero() {
		return 0, true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, false
	}
	return remaining, true
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
	return hp.acquireWithDeadline(time.Time{})
}

func (hp *hostPool) acquireWithDeadline(deadline time.Time) (*Session, error) {
	// Try to get an idle session
	select {
	case sess := <-hp.idle:
		pingTimeout := 2 * time.Second
		if remaining, ok := timeoutRemaining(deadline); !ok {
			sess.close()
			return nil, fmt.Errorf("SSH connection to %s timed out", hp.host)
		} else if remaining > 0 && remaining < pingTimeout {
			pingTimeout = remaining
		}
		if sess.alive && sess.pingWithTimeout(pingTimeout) {
			return sess, nil
		}
		sess.close()
		// fall through to create new
	default:
	}

	// Create a new session
	return hp.newSessionWithDeadline(deadline)
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
	return hp.newSessionWithDeadline(time.Time{})
}

func (hp *hostPool) newSessionWithDeadline(deadline time.Time) (*Session, error) {
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
	sshArgs = append(sshArgs, identityArgs(hp.host)...)
	sshArgs = append(sshArgs, hostTarget(hp.host), remoteCmd)
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

	readyTimeout := defaultReadyTimeout
	if remaining, ok := timeoutRemaining(deadline); !ok {
		killAndWait(cmd)
		return nil, fmt.Errorf("SSH connection to %s timed out", hp.host)
	} else if remaining > 0 && remaining < readyTimeout {
		readyTimeout = remaining
	}

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
	case <-time.After(readyTimeout):
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
