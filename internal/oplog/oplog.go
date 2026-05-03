// Package oplog provides structured operation logging for forensic debugging.
// All significant operations (job start, kill, queue, sync) are logged to a
// persistent JSONL file that can be queried to understand what happened.
package oplog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	OpAgentStart      = "agent.start"
	OpAgentStop       = "agent.stop"
	OpAgentVersion    = "agent.version"
	OpAgentHeartbeat  = "agent.heartbeat"

	// Coordinator operations
	OpCoordinatorStart    = "coordinator.start"
	OpCoordinatorStop     = "coordinator.stop"
	OpCoordinatorDispatch = "coordinator.dispatched"
	OpCoordinatorDeferred = "coordinator.deferred"
	OpCoordinatorRetry    = "coordinator.retry"
	OpCoordinatorError    = "coordinator.error"

	// Placement telemetry
	OpPlacementDecided = "placement.decided"
	OpHostMetrics      = "host.metrics"

	// Auto-remediation operations
	OpCoordinatorDiagnosis   = "coordinator.diagnosis"
	OpCoordinatorRemediation = "coordinator.remediation"
	OpCoordinatorAgentInvoke = "coordinator.agent.invoke"

	// Agent R2/cloud operations
	OpR2Get    = "r2.get"
	OpR2Put    = "r2.put"
	OpR2Delete = "r2.delete"
	OpR2Copy   = "r2.copy" // rclone copy (bulk upload)

	// Cloud source upload and job assignment
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
	l.write(&entry)
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
	decoder := json.NewDecoder(file)
	for decoder.More() {
		var entry Entry
		if err := decoder.Decode(&entry); err != nil {
			continue // Skip malformed entries
		}
		entries = append(entries, entry)
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

// WriteSyncedInstanceLog stores a cached copy of an instance ops log.
func WriteSyncedInstanceLog(instanceID int64, data []byte) error {
	if err := os.MkdirAll(SyncedInstanceLogDir(), 0755); err != nil {
		return fmt.Errorf("create synced ops log dir: %w", err)
	}
	return os.WriteFile(SyncedInstanceLogPath(instanceID), data, 0644)
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
