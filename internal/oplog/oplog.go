// Package oplog provides structured operation logging for forensic debugging.
// All significant operations (job start, kill, queue, sync) are logged to a
// persistent JSONL file that can be queried to understand what happened.
package oplog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Operation type constants for semantic logging
const (
	OpJobStart        = "job.start"
	OpJobStartFailed  = "job.start.failed"
	OpJobStarted      = "job.started"
	OpJobKill         = "job.kill"
	OpJobKilled       = "job.killed"
	OpJobRestart      = "job.restart"
	OpJobQueue        = "job.queue"
	OpJobCancel       = "job.cancel"
	OpJobSync         = "job.sync"
	OpJobProbe        = "job.probe"
	OpJobDead         = "job.dead"
	OpJobComplete     = "job.complete"
	OpJobFail         = "job.fail"
	OpPhaseTransition = "phase.transition"
	OpQueueStart      = "queue.start"
	OpQueueStop       = "queue.stop"
	OpHostConnect     = "host.connect"
	OpHostTimeout     = "host.timeout"
	OpDeferred        = "op.deferred"
	OpDeferredExec    = "op.deferred.exec"
	OpTUIAction       = "tui.action"
	OpCLICommand      = "cli.command"
	OpSSH             = "ssh.exec"
	OpAgentStart      = "agent.start"
	OpAgentStop       = "agent.stop"
	OpAgentVersion    = "agent.version"
	OpAgentHeartbeat  = "agent.heartbeat"
	OpAgentPanic      = "agent.panic"

	// Placement telemetry
	OpPlacementDecided = "placement.decided"
	OpHostMetrics      = "host.metrics"

	// Auto-remediation operations
	OpRemediationDiagnosis = "remediation.diagnosis"
	OpRemediationApplied   = "remediation.applied"

	// Agent R2/cloud operations
	OpR2Get    = "r2.get"
	OpR2Put    = "r2.put"
	OpR2Delete = "r2.delete"
	OpR2Copy   = "r2.copy" // rclone copy (bulk upload)

	// Cloud source upload and job assignment
	OpR2UploadAgent       = "r2.upload_agent"
	OpR2UploadSource      = "r2.upload_source"
	OpCloudSetJobInstance = "cloud.set_job_instance"

	// Cloud launch telemetry
	OpLaunchLaunchRequested = "cloud.instance.launch.requested"
	OpLaunchLaunchCreated   = "cloud.instance.launch.created"
	OpLaunchLaunchPhase     = "cloud.instance.launch.phase"
	OpLaunchLaunchFailed    = "cloud.instance.launch.failed"
	OpLaunchLaunchReadback  = "cloud.instance.launch.readback"
	OpLaunchLaunchMismatch  = "cloud.instance.launch.mismatch"
	OpLaunchDestroyFailed   = "cloud.instance.destroy.failed"
	OpLaunchPaused          = "cloud.instance.launch.paused"
	OpLaunchResumed         = "cloud.instance.launch.resumed"
	OpLaunchTerminated      = "cloud.instance.launch.terminated"
)

// Entry represents a single log entry in JSONL format.
// Fields use short names to minimize log file size.
type Entry struct {
	Time      time.Time `json:"t"`
	Operation string    `json:"op"`
	JobID     int64     `json:"job,omitempty"`
	Host      string    `json:"host,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	Error     string    `json:"err,omitempty"`
	Duration  int64     `json:"dur_ms,omitempty"` // milliseconds
	Audit     *Audit    `json:"audit,omitempty"`
}

// Audit records local operator context for user-initiated mutations.
type Audit struct {
	Source     string   `json:"source,omitempty"` // cli, tui, daemon, agent
	CWD        string   `json:"cwd,omitempty"`
	Argv       []string `json:"argv,omitempty"`
	Agent      string   `json:"agent,omitempty"`
	AgentEnv   string   `json:"agent_env,omitempty"`
	SessionEnv string   `json:"session_env,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
}

// Logger is the interface for operation logging.
type Logger interface {
	Log(op string, opts ...Option)
	LogJob(op string, jobID int64, host string, opts ...Option)
	Sync() error
	Close() error
}

// Option configures a log entry.
type Option func(*Entry)

// WithDetail adds detail text to the entry.
func WithDetail(detail string) Option {
	return func(e *Entry) {
		e.Detail = detail
	}
}

// WithDetailf adds formatted detail text to the entry.
func WithDetailf(format string, args ...any) Option {
	return func(e *Entry) {
		e.Detail = fmt.Sprintf(format, args...)
	}
}

// WithError adds an error to the entry.
func WithError(err error) Option {
	return func(e *Entry) {
		if err != nil {
			e.Error = err.Error()
		}
	}
}

// WithErrorStr adds an error string to the entry.
func WithErrorStr(errStr string) Option {
	return func(e *Entry) {
		e.Error = errStr
	}
}

// WithDuration adds a duration to the entry.
func WithDuration(d time.Duration) Option {
	return func(e *Entry) {
		e.Duration = d.Milliseconds()
	}
}

// WithHost adds a host to the entry (for non-job operations).
func WithHost(host string) Option {
	return func(e *Entry) {
		e.Host = host
	}
}

// WithJobID adds a job ID to the entry (for non-job operations).
func WithJobID(jobID int64) Option {
	return func(e *Entry) {
		e.JobID = jobID
	}
}

// WithAudit records explicit audit context on an operation.
func WithAudit(a Audit) Option {
	return func(e *Entry) {
		e.Audit = &a
	}
}

// WithAuditSource captures the current process context for a command source.
func WithAuditSource(source string) Option {
	return WithAudit(CaptureAudit(source))
}

// CaptureAudit collects non-secret local process context for forensic audit
// entries. It intentionally records only a small allowlist of agent/session
// environment variables.
func CaptureAudit(source string) Audit {
	wd, _ := os.Getwd()
	a := Audit{
		Source: source,
		CWD:    wd,
		Argv:   append([]string(nil), os.Args...),
	}
	if agent, envName, sessionEnv, sessionID := detectCodingAgent(); agent != "" {
		a.Agent = agent
		a.AgentEnv = envName
		a.SessionEnv = sessionEnv
		a.SessionID = sessionID
	}
	return a
}

func detectCodingAgent() (agent, agentEnv, sessionEnv, sessionID string) {
	for _, candidate := range []struct {
		env   string
		agent string
	}{
		{"CODEX_CI", "codex"},
		{"CODEX_SANDBOX", "codex"},
		{"CODEX_SESSION_ID", "codex"},
		{"CLAUDECODE", "claude-code"},
		{"CLAUDE_CODE", "claude-code"},
		{"CLAUDE_SESSION_ID", "claude-code"},
	} {
		if v := strings.TrimSpace(os.Getenv(candidate.env)); v != "" {
			agent = candidate.agent
			agentEnv = candidate.env
			break
		}
	}
	for _, envName := range []string{"CODEX_SESSION_ID", "CLAUDE_SESSION_ID", "CLAUDECODE_SESSION_ID", "WEFT_AGENT_SESSION"} {
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
			sessionEnv = envName
			sessionID = v
			break
		}
	}
	return agent, agentEnv, sessionEnv, sessionID
}

// fileLogger writes entries to a JSONL file.
type fileLogger struct {
	mu      sync.Mutex
	file    *os.File
	path    string
	maxSize int64
	encoder *json.Encoder
}

// noopLogger is a no-op logger used when logging is disabled.
type noopLogger struct{}

func (noopLogger) Log(op string, opts ...Option)                              {}
func (noopLogger) LogJob(op string, jobID int64, host string, opts ...Option) {}
func (noopLogger) Sync() error                                                { return nil }
func (noopLogger) Close() error                                               { return nil }

var (
	defaultLogger Logger = &noopLogger{}
	defaultMu     sync.RWMutex
)

// DefaultLogPath returns the default path for the operations log.
func DefaultLogPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "weft", "operations.log")
}

// DefaultMaxSize is the default maximum log file size (10MB).
const DefaultMaxSize = 10 * 1024 * 1024

// Init initializes the global logger with the given path and max size.
// If maxSize is 0, DefaultMaxSize is used.
// This should be called early in program startup.
func Init(logPath string, maxSize int64) error {
	if logPath == "" {
		logPath = DefaultLogPath()
	}
	if maxSize == 0 {
		maxSize = DefaultMaxSize
	}

	logger, err := newFileLogger(logPath, maxSize)
	if err != nil {
		return err
	}

	defaultMu.Lock()
	defaultLogger = logger
	defaultMu.Unlock()

	return nil
}

// Close closes the global logger.
func Close() error {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	err := defaultLogger.Close()
	defaultLogger = &noopLogger{}
	return err
}

// Sync flushes buffered writes for the global logger without disabling it.
func Sync() error {
	defaultMu.RLock()
	l := defaultLogger
	defaultMu.RUnlock()
	return l.Sync()
}

// Log logs an operation using the global logger.
func Log(op string, opts ...Option) {
	defaultMu.RLock()
	l := defaultLogger
	defaultMu.RUnlock()
	l.Log(op, opts...)
}

// LogJob logs a job operation using the global logger.
func LogJob(op string, jobID int64, host string, opts ...Option) {
	defaultMu.RLock()
	l := defaultLogger
	defaultMu.RUnlock()
	l.LogJob(op, jobID, host, opts...)
}

func newFileLogger(path string, maxSize int64) (*fileLogger, error) {
	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	// Check for rotation
	if err := rotateIfNeeded(path, maxSize); err != nil {
		return nil, fmt.Errorf("rotate log: %w", err)
	}

	// Open file for append
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	return &fileLogger{
		file:    file,
		path:    path,
		maxSize: maxSize,
		encoder: json.NewEncoder(file),
	}, nil
}

func rotateIfNeeded(path string, maxSize int64) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil // No file to rotate
	}
	if err != nil {
		return err
	}

	if info.Size() < maxSize {
		return nil // File is under size limit
	}

	// Rotate: remove .1, rename current to .1
	backup := path + ".1"
	os.Remove(backup) // Ignore error if doesn't exist
	if err := os.Rename(path, backup); err != nil {
		return fmt.Errorf("rename to backup: %w", err)
	}

	return nil
}

func (l *fileLogger) Log(op string, opts ...Option) {
	entry := Entry{
		Time:      time.Now(),
		Operation: op,
	}
	for _, opt := range opts {
		opt(&entry)
	}
	enrichAudit(&entry)
	l.write(&entry)
}

func (l *fileLogger) LogJob(op string, jobID int64, host string, opts ...Option) {
	entry := Entry{
		Time:      time.Now(),
		Operation: op,
		JobID:     jobID,
		Host:      host,
	}
	for _, opt := range opts {
		opt(&entry)
	}
	enrichAudit(&entry)
	l.write(&entry)
}

func enrichAudit(entry *Entry) {
	if entry.Audit != nil {
		return
	}
	switch entry.Operation {
	case OpCLICommand:
		audit := CaptureAudit("cli")
		entry.Audit = &audit
	case OpTUIAction:
		audit := CaptureAudit("tui")
		entry.Audit = &audit
	}
}

func (l *fileLogger) write(entry *Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return
	}

	// Encode directly to file (encoder handles newlines in JSONL)
	l.encoder.Encode(entry)
}

func (l *fileLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return nil
	}

	err := l.file.Close()
	l.file = nil
	return err
}

func (l *fileLogger) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return nil
	}
	return l.file.Sync()
}

// ReadEntries reads all entries from a log file.
// Returns entries in chronological order.
func ReadEntries(path string) ([]Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var entries []Entry
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimSpace(line)
		}
		if len(line) > 0 {
			var entry Entry
			if err := json.Unmarshal(line, &entry); err == nil {
				entries = append(entries, entry)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	return entries, nil
}

// ReadRecent returns the last n entries from the default log file.
// Returns entries in chronological order (oldest first).
func ReadRecent(n int) ([]Entry, error) {
	return ReadRecentFrom(DefaultLogPath(), n)
}

// ReadRecentFrom returns the last n entries from the given log file.
func ReadRecentFrom(path string, n int) ([]Entry, error) {
	entries, err := ReadEntries(path)
	if err != nil {
		return nil, err
	}
	if n <= 0 || len(entries) <= n {
		return entries, nil
	}
	return entries[len(entries)-n:], nil
}

// SyncedInstanceLogDir returns the directory used for cached instance ops logs.
func SyncedInstanceLogDir() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/tmp"
	}
	return filepath.Join(home, ".cache", "weft", "cloud-instance-ops")
}

// SyncedInstanceLogPath returns the cache path for an instance ops log.
func SyncedInstanceLogPath(instanceID int64) string {
	return filepath.Join(SyncedInstanceLogDir(), fmt.Sprintf("%d.jsonl", instanceID))
}

// WriteSyncedInstanceLog stores a cached copy of an instance ops log,
// overwriting any existing file. Used for the initial sync and any
// fall-back when the server-side log was rewritten.
func WriteSyncedInstanceLog(instanceID int64, data []byte) error {
	if err := os.MkdirAll(SyncedInstanceLogDir(), 0755); err != nil {
		return fmt.Errorf("create synced ops log dir: %w", err)
	}
	return os.WriteFile(SyncedInstanceLogPath(instanceID), data, 0644)
}

// AppendSyncedInstanceLog appends bytes to the cached opslog. Returns
// the cache file's size after the append (used by the caller as the
// next byte offset for the following sync). Creates the file if it
// doesn't exist yet.
func AppendSyncedInstanceLog(instanceID int64, data []byte) (int64, error) {
	if err := os.MkdirAll(SyncedInstanceLogDir(), 0755); err != nil {
		return 0, fmt.Errorf("create synced ops log dir: %w", err)
	}
	path := SyncedInstanceLogPath(instanceID)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return 0, fmt.Errorf("open synced ops log %s: %w", path, err)
	}
	defer f.Close()
	if len(data) > 0 {
		if _, err := f.Write(data); err != nil {
			return 0, fmt.Errorf("append synced ops log: %w", err)
		}
	}
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat synced ops log: %w", err)
	}
	return info.Size(), nil
}

// SyncedInstanceLogSize returns the byte length of the cached opslog,
// or (0, nil) if the file doesn't exist yet.
func SyncedInstanceLogSize(instanceID int64) (int64, error) {
	info, err := os.Stat(SyncedInstanceLogPath(instanceID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return info.Size(), nil
}

// ListSyncedInstanceLogPaths returns cached instance ops log paths.
func ListSyncedInstanceLogPaths() ([]string, error) {
	entries, err := os.ReadDir(SyncedInstanceLogDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		paths = append(paths, filepath.Join(SyncedInstanceLogDir(), entry.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

// FilterOptions for filtering log entries.
type FilterOptions struct {
	JobID      int64
	Host       string
	Operation  string
	Since      time.Time
	Until      time.Time
	ErrorsOnly bool
}

// FilterEntries filters entries based on options.
func FilterEntries(entries []Entry, opts FilterOptions) []Entry {
	var result []Entry
	for _, e := range entries {
		if opts.JobID != 0 && e.JobID != opts.JobID {
			continue
		}
		if opts.Host != "" && e.Host != opts.Host {
			continue
		}
		if opts.Operation != "" && e.Operation != opts.Operation {
			continue
		}
		if !opts.Since.IsZero() && e.Time.Before(opts.Since) {
			continue
		}
		if !opts.Until.IsZero() && e.Time.After(opts.Until) {
			continue
		}
		if opts.ErrorsOnly && e.Error == "" {
			continue
		}
		result = append(result, e)
	}
	return result
}
