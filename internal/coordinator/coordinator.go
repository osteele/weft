// Package coordinator implements the coordinator daemon that centralizes
// job placement decisions. It watches for intent files, scores hosts, and
// dispatches jobs to remote queue runners.
package coordinator

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/ssh"
)

// Config holds coordinator daemon configuration.
type Config struct {
	IntentDir     string        // where intent files are written (default: ~/.cache/weft/intents/)
	ArchiveDir    string        // where processed intents are moved (default: ~/.cache/weft/intents/archive/)
	PIDFile       string        // PID file path (default: ~/.cache/weft/coordinator.pid)
	LogPath       string        // operation log path
	PollInterval  time.Duration // host state polling interval (default: 30s)
	RetryInterval time.Duration // retry queue drain interval (default: 15s)
	SyncInterval  time.Duration // full host sync interval (default: 60s)
}

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cache", "weft")
	return Config{
		IntentDir:     filepath.Join(cacheDir, "intents"),
		ArchiveDir:    filepath.Join(cacheDir, "intents", "archive"),
		PIDFile:       filepath.Join(cacheDir, "coordinator.pid"),
		LogPath:       filepath.Join(cacheDir, "coordinator-operations.log"),
		PollInterval:  30 * time.Second,
		RetryInterval: 15 * time.Second,
		SyncInterval:  60 * time.Second,
	}
}

// Coordinator is the central daemon that processes placement intents.
type Coordinator struct {
	db         *sql.DB
	config     Config
	hostState  map[string]*HostState
	retryQueue *retryQueue
	processed  map[string]bool // idempotency: intent_id -> processed
	mu         sync.Mutex
	logger     *log.Logger
}

// New creates a new Coordinator.
func New(database *sql.DB, config Config) *Coordinator {
	return &Coordinator{
		db:         database,
		config:     config,
		hostState:  make(map[string]*HostState),
		retryQueue: newRetryQueue(),
		processed:  make(map[string]bool),
		logger:     log.New(os.Stderr, "[coordinator] ", log.LstdFlags),
	}
}

// Run starts the coordinator event loop. It blocks until the context is cancelled.
func (c *Coordinator) Run(ctx context.Context) error {
	// Write PID file
	if err := c.writePIDFile(); err != nil {
		return fmt.Errorf("write PID file: %w", err)
	}
	defer os.Remove(c.config.PIDFile)

	// Initialize oplog
	if err := oplog.Init(c.config.LogPath, oplog.DefaultMaxSize); err != nil {
		c.logger.Printf("oplog init failed (continuing): %v", err)
	}
	defer oplog.Close()

	oplog.Log(oplog.OpCoordinatorStart)
	c.logger.Println("coordinator started")

	// Seed host state from inventory so all known hosts are tracked from the start
	c.seedHostState()

	// Ensure intent and archive directories exist
	for _, dir := range []string{c.config.IntentDir, c.config.ArchiveDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	// Process existing intent files (catch-up after restart)
	existing, err := scanExistingIntents(c.config.IntentDir)
	if err != nil {
		c.logger.Printf("scan existing intents: %v", err)
	}
	for _, path := range existing {
		c.handleIntentFile(path)
	}

	// Start fsnotify watcher
	done := make(chan struct{})
	defer close(done)

	intentCh, err := startWatcher(c.config.IntentDir, done)
	if err != nil {
		return fmt.Errorf("start watcher: %w", err)
	}

	pollTicker := time.NewTicker(c.config.PollInterval)
	defer pollTicker.Stop()

	retryTicker := time.NewTicker(c.config.RetryInterval)
	defer retryTicker.Stop()

	syncTicker := time.NewTicker(c.config.SyncInterval)
	defer syncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			oplog.Log(oplog.OpCoordinatorStop)
			c.logger.Println("coordinator stopped")
			return nil

		case path, ok := <-intentCh:
			if !ok {
				return fmt.Errorf("watcher channel closed unexpectedly")
			}
			c.handleIntentFile(path)

		case <-pollTicker.C:
			c.probeHosts()

		case <-retryTicker.C:
			c.drainRetryQueue()

		case <-syncTicker.C:
			c.syncAllHosts()
		}
	}
}

// handleIntentFile processes a single intent file.
func (c *Coordinator) handleIntentFile(path string) {
	i, err := intent.ParseFile(path)
	if err != nil {
		c.logger.Printf("parse intent %s: %v", path, err)
		oplog.Log(oplog.OpCoordinatorError, oplog.WithDetailf("parse %s", filepath.Base(path)), oplog.WithError(err))
		return
	}

	// Idempotency check
	c.mu.Lock()
	if c.processed[i.IntentID] {
		c.mu.Unlock()
		c.logger.Printf("skip duplicate intent %s", i.IntentID)
		c.archiveIntent(path)
		return
	}
	c.processed[i.IntentID] = true
	c.mu.Unlock()

	// Resolve host
	host, reasons, err := resolveHost(c.db, i)
	if err != nil {
		c.logger.Printf("resolve host for intent %s: %v", i.IntentID, err)
		oplog.Log(oplog.OpCoordinatorError, oplog.WithDetailf("resolve host intent=%s", i.IntentID), oplog.WithError(err))
		return
	}

	c.logger.Printf("intent %s -> host %s (%s)", i.IntentID, host, strings.Join(reasons, "; "))

	// Check if host is online
	c.mu.Lock()
	hs := c.hostState[host]
	hostOnline := hs != nil && hs.Online
	c.mu.Unlock()

	if !hostOnline {
		// Probe host before giving up
		if !c.probeHost(host) {
			c.logger.Printf("host %s offline, deferring intent %s", host, i.IntentID)
			oplog.Log(oplog.OpCoordinatorDeferred, oplog.WithHost(host), oplog.WithDetailf("intent=%s", i.IntentID))
			c.retryQueue.Add(i, host)
			c.archiveIntent(path)
			return
		}
	}

	// Pre-stage missing inputs (best-effort, does not block dispatch)
	prestageInputs(c.db, i, host, c.logger)

	// Dispatch
	jobID, err := dispatchIntent(c.db, i, host)
	if err != nil {
		// Check if it's a connection error — defer for retry
		if isSSHConnectionError(err) {
			c.logger.Printf("host %s unreachable during dispatch, deferring intent %s", host, i.IntentID)
			oplog.Log(oplog.OpCoordinatorDeferred, oplog.WithHost(host), oplog.WithDetailf("intent=%s dispatch_err", i.IntentID))
			c.retryQueue.Add(i, host)
			c.markHostOffline(host)
		} else {
			c.logger.Printf("dispatch intent %s to %s: %v", i.IntentID, host, err)
			oplog.Log(oplog.OpCoordinatorError, oplog.WithHost(host), oplog.WithDetailf("dispatch intent=%s", i.IntentID), oplog.WithError(err))
		}
		c.archiveIntent(path)
		return
	}

	c.logger.Printf("dispatched intent %s as job %d on %s", i.IntentID, jobID, host)
	oplog.LogJob(oplog.OpCoordinatorDispatch, jobID, host, oplog.WithDetailf("intent=%s reasons=%s", i.IntentID, strings.Join(reasons, "; ")))
	c.archiveIntent(path)
	c.writeOutcome(i.IntentID, jobID, host, reasons, "")
}

// probeHosts checks connectivity to all known hosts.
func (c *Coordinator) probeHosts() {
	c.mu.Lock()
	hosts := make([]string, 0, len(c.hostState))
	for name := range c.hostState {
		hosts = append(hosts, name)
	}
	c.mu.Unlock()

	for _, host := range hosts {
		c.probeHost(host)
	}
}

// probeHost checks if a host is reachable via SSH. Returns true if online.
func (c *Coordinator) probeHost(host string) bool {
	_, _, err := ssh.RunWithTimeout(host, "true", 5*time.Second)
	online := err == nil

	c.mu.Lock()
	hs, ok := c.hostState[host]
	if !ok {
		hs = &HostState{Name: host}
		c.hostState[host] = hs
	}
	wasOnline := hs.Online
	hs.Online = online
	hs.LastProbe = time.Now()
	if online {
		hs.LastOnline = time.Now()
	}
	c.mu.Unlock()

	if online && !wasOnline {
		c.logger.Printf("host %s came online", host)
		oplog.Log(oplog.OpHostConnect, oplog.WithHost(host))
	} else if !online && wasOnline {
		c.logger.Printf("host %s went offline", host)
		oplog.Log(oplog.OpHostTimeout, oplog.WithHost(host))
	}

	return online
}

// seedHostState populates the host state map from the embedded inventory
// and probes each host for initial connectivity.
func (c *Coordinator) seedHostState() {
	hosts, err := inventory.LoadEmbeddedHosts()
	if err != nil {
		c.logger.Printf("load inventory: %v", err)
		return
	}

	c.mu.Lock()
	for _, h := range hosts {
		if _, ok := c.hostState[h.Name]; !ok {
			c.hostState[h.Name] = &HostState{Name: h.Name}
		}
	}
	c.mu.Unlock()

	// Probe all hosts in parallel for fast startup
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			c.probeHost(name)
		}(h.Name)
	}
	wg.Wait()

	// Log results
	c.mu.Lock()
	var online, offline []string
	for name, hs := range c.hostState {
		if hs.Online {
			online = append(online, name)
		} else {
			offline = append(offline, name)
		}
	}
	c.mu.Unlock()

	c.logger.Printf("hosts online: %v, offline: %v", online, offline)
}

// markHostOffline updates a host's state to offline.
func (c *Coordinator) markHostOffline(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hs, ok := c.hostState[host]
	if !ok {
		hs = &HostState{Name: host}
		c.hostState[host] = hs
	}
	hs.Online = false
	hs.LastProbe = time.Now()
}

// drainRetryQueue attempts to dispatch deferred intents for hosts that are now online.
func (c *Coordinator) drainRetryQueue() {
	if c.retryQueue.Len() == 0 {
		return
	}

	// Get list of online hosts
	c.mu.Lock()
	onlineHosts := make(map[string]bool)
	for name, hs := range c.hostState {
		if hs.Online {
			onlineHosts[name] = true
		}
	}
	c.mu.Unlock()

	for host := range onlineHosts {
		items := c.retryQueue.DrainForHost(host)
		for _, item := range items {
			c.logger.Printf("retrying intent %s on %s", item.intent.IntentID, host)
			oplog.Log(oplog.OpCoordinatorRetry, oplog.WithHost(host), oplog.WithDetailf("intent=%s", item.intent.IntentID))

			jobID, err := dispatchIntent(c.db, item.intent, host)
			if err != nil {
				if isSSHConnectionError(err) {
					c.retryQueue.Add(item.intent, host)
					c.markHostOffline(host)
					break // stop retrying this host
				}
				c.logger.Printf("retry dispatch intent %s: %v", item.intent.IntentID, err)
				oplog.Log(oplog.OpCoordinatorError, oplog.WithHost(host), oplog.WithDetailf("retry intent=%s", item.intent.IntentID), oplog.WithError(err))
				continue
			}
			c.logger.Printf("retry dispatched intent %s as job %d on %s", item.intent.IntentID, jobID, host)
			oplog.LogJob(oplog.OpCoordinatorDispatch, jobID, host, oplog.WithDetailf("retry intent=%s", item.intent.IntentID))
		}
	}
}

// syncAllHosts runs SyncHost for each known host.
func (c *Coordinator) syncAllHosts() {
	if c.db == nil {
		return
	}

	c.mu.Lock()
	hosts := make([]string, 0, len(c.hostState))
	for name, hs := range c.hostState {
		if hs.Online {
			hosts = append(hosts, name)
		}
	}
	c.mu.Unlock()

	for _, host := range hosts {
		_, err := ops.SyncHost(c.db, host, ops.HostSyncOptions{
			Timeout: 30 * time.Second,
		}, nil)
		if err != nil {
			c.logger.Printf("sync %s: %v", host, err)
		}
	}
}

// writeOutcome writes a placement outcome file so the submitting laptop can
// learn which host was chosen.
func (c *Coordinator) writeOutcome(intentID string, jobID int64, host string, reasons []string, errMsg string) {
	outcome := &intent.Outcome{
		IntentID: intentID,
		JobID:    jobID,
		Host:     host,
		Reasons:  reasons,
		Error:    errMsg,
		Time:     time.Now(),
	}
	if err := intent.WriteOutcome(c.config.ArchiveDir, outcome); err != nil {
		c.logger.Printf("write outcome for %s: %v", intentID, err)
	}
}

// archiveIntent moves a processed intent file to the archive directory.
func (c *Coordinator) archiveIntent(path string) {
	name := filepath.Base(path)
	dest := filepath.Join(c.config.ArchiveDir, name)
	if err := os.Rename(path, dest); err != nil {
		// May fail if already archived or file disappeared
		c.logger.Printf("archive intent %s: %v", name, err)
	}
}

// writePIDFile writes the current process PID to the PID file.
func (c *Coordinator) writePIDFile() error {
	dir := filepath.Dir(c.config.PIDFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(c.config.PIDFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
}

// HostStates returns a snapshot of all tracked host states.
func (c *Coordinator) HostStates() map[string]HostState {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]HostState, len(c.hostState))
	for name, hs := range c.hostState {
		result[name] = *hs
	}
	return result
}

// RetryQueueLen returns the number of items in the retry queue.
func (c *Coordinator) RetryQueueLen() int {
	return c.retryQueue.Len()
}

// isSSHConnectionError checks if an error is an SSH connection error.
func isSSHConnectionError(err error) bool {
	if err == nil {
		return false
	}
	return ssh.IsConnectionError(err.Error())
}
