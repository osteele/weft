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
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinator/services"
	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/vastai"
)

// Config holds coordinator daemon configuration.
type Config struct {
	IntentDir           string        // where intent files are written (default: ~/.cache/weft/intents/)
	ArchiveDir          string        // where processed intents are moved (default: ~/.cache/weft/intents/archive/)
	PIDFile             string        // PID file path (default: ~/.cache/weft/coordinator.pid)
	LogPath             string        // operation log path
	PollInterval        time.Duration // host state polling interval (default: 30s)
	RetryInterval       time.Duration // retry queue drain interval (default: 15s)
	SyncInterval        time.Duration // full host sync interval (default: 60s)
	RemediationInterval time.Duration // failed job diagnosis interval (default: 30s)
}

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cache", "weft")
	return Config{
		IntentDir:           filepath.Join(cacheDir, "intents"),
		ArchiveDir:          filepath.Join(cacheDir, "intents", "archive"),
		PIDFile:             filepath.Join(cacheDir, "coordinator.pid"),
		LogPath:             filepath.Join(cacheDir, "coordinator-operations.log"),
		PollInterval:        30 * time.Second,
		RetryInterval:       15 * time.Second,
		SyncInterval:        60 * time.Second,
		RemediationInterval: 30 * time.Second,
	}
}

// Coordinator is the central daemon that processes placement intents.
type Coordinator struct {
	db         *sql.DB
	config     Config
	appConfig  *config.Config
	hostState  *services.HostStateManager
	retryQueue *services.RetryQueue
	logger     *log.Logger

	// Composable services
	prober       *services.HostProber
	syncer       *services.HostSyncer
	remediator   *services.Remediator
	retryDrainer *services.RetryDrainer

	// CloudClients holds the cloud provider clients. If nil, defaults are created.
	CloudClients []cloud.Client

	// VastaiClient is the Vast.ai API client (legacy). If nil, vastaiClient() creates one.
	VastaiClient vastai.VastaiClient
}

// cloudClient returns a cloud.Client for the given provider, or nil if not available.
func (c *Coordinator) cloudClient(provider string) cloud.Client {
	for _, cl := range c.CloudClients {
		if string(cl.Provider()) == provider {
			return cl
		}
	}
	// Fallback: create from VastaiClient field or default
	if provider == string(cloud.ProviderVastai) {
		inner := c.VastaiClient
		if inner == nil {
			inner = vastai.NewClient()
		}
		return vastai.NewCloudClient(inner)
	}
	return nil
}

// vastaiClient returns the configured VastaiClient or creates a default one.
func (c *Coordinator) vastaiClient() vastai.VastaiClient {
	if c.VastaiClient != nil {
		return c.VastaiClient
	}
	return vastai.NewClient()
}

// New creates a new Coordinator.
func New(database *sql.DB, cfg Config) *Coordinator {
	appCfg, _ := config.Load()
	logger := log.New(os.Stderr, "[coordinator] ", log.LstdFlags)
	hostState := services.NewHostStateManager(logger)
	retryQueue := services.NewRetryQueue()

	c := &Coordinator{
		db:         database,
		config:     cfg,
		appConfig:  appCfg,
		hostState:  hostState,
		retryQueue: retryQueue,
		logger:     logger,
	}

	c.prober = services.NewHostProber(hostState, cfg.PollInterval)
	c.syncer = services.NewHostSyncer(database, hostState, logger, cfg.SyncInterval)
	c.remediator = services.NewRemediator(database, logger, appCfg, cfg.RemediationInterval)
	c.retryDrainer = services.NewRetryDrainer(retryQueue, hostState, c.dispatchIntent, logger, cfg.RetryInterval)

	return c
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

	// Initialize processed intents table for crash-safe idempotency
	if err := initProcessedTable(c.db); err != nil {
		return fmt.Errorf("init processed_intents table: %w", err)
	}
	// Clean up entries older than 7 days
	_ = cleanupOldProcessed(c.db, 7*24*time.Hour)

	oplog.Log(oplog.OpCoordinatorStart)
	c.logger.Println("coordinator started")

	// Seed host state from inventory so all known hosts are tracked from the start.
	// Run in a goroutine so the event loop starts immediately while probes complete.
	c.hostState.SeedFromInventory()
	go c.prober.ProbeAllParallel()

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

	// Start composable background services
	svcCtx, svcCancel := context.WithCancel(ctx)
	defer svcCancel()
	go c.prober.Start(svcCtx)
	go c.syncer.Start(svcCtx)
	go c.remediator.Start(svcCtx)
	go c.retryDrainer.Start(svcCtx)

	vastaiSweepTicker := time.NewTicker(60 * time.Second)
	defer vastaiSweepTicker.Stop()

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

		case <-vastaiSweepTicker.C:
			c.sweepVastaiResults()
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

	// Idempotency check (persisted to DB for crash safety)
	if isProcessed(c.db, i.IntentID) {
		c.logger.Printf("skip duplicate intent %s", i.IntentID)
		c.archiveIntent(path)
		return
	}
	if err := markProcessed(c.db, i.IntentID); err != nil {
		c.logger.Printf("mark intent %s processed: %v", i.IntentID, err)
	}

	// Resolve host
	host, reasons, err := resolveHost(c.db, i, c.appConfig)
	if err != nil {
		c.logger.Printf("resolve host for intent %s: %v", i.IntentID, err)
		oplog.Log(oplog.OpCoordinatorError, oplog.WithDetailf("resolve host intent=%s", i.IntentID), oplog.WithError(err))
		return
	}

	c.logger.Printf("intent %s -> host %s (%s)", i.IntentID, host, strings.Join(reasons, "; "))

	// Check if host is online
	if !c.hostState.IsOnline(host) {
		// Probe host before giving up
		if !c.hostState.ProbeHost(host) {
			c.logger.Printf("host %s offline, deferring intent %s", host, i.IntentID)
			oplog.Log(oplog.OpCoordinatorDeferred, oplog.WithHost(host), oplog.WithDetailf("intent=%s", i.IntentID))
			c.retryQueue.Add(i, host)
			c.archiveIntent(path)
			return
		}
	}

	// Pre-stage missing inputs (best-effort, does not block dispatch)
	prestageInputs(c.db, i, host, c.logger)

	// Sync sources from coordinator to target host
	syncSources(i, host, c.logger)

	// Dispatch
	jobID, err := dispatchIntent(c.db, i, host)
	if err != nil {
		// Check if it's a connection error — defer for retry
		if isSSHConnectionError(err) {
			c.logger.Printf("host %s unreachable during dispatch, deferring intent %s", host, i.IntentID)
			oplog.Log(oplog.OpCoordinatorDeferred, oplog.WithHost(host), oplog.WithDetailf("intent=%s dispatch_err", i.IntentID))
			c.retryQueue.Add(i, host)
			c.hostState.MarkOffline(host)
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

// dispatchIntent dispatches an intent to a host. Used as a callback for RetryDrainer.
func (c *Coordinator) dispatchIntent(i *intent.Intent, host string) (int64, error) {
	return dispatchIntent(c.db, i, host)
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
func (c *Coordinator) HostStates() map[string]services.HostState {
	return c.hostState.Snapshot()
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
