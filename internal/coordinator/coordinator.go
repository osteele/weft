// Package coordinator implements the coordinator daemon that manages
// cloud instance sweeps and host state monitoring.
package coordinator

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinator/services"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/slack"
	"github.com/osteele/weft/internal/vastai"
)

// Config holds coordinator daemon configuration.
type Config struct {
	PIDFile             string        // PID file path (default: ~/.cache/weft/coordinator.pid)
	LogPath             string        // operation log path
	PollInterval        time.Duration // host state polling interval (default: 30s)
	SyncInterval        time.Duration // full host sync interval (default: 60s)
	RemediationInterval time.Duration // failed job diagnosis interval (default: 30s)
}

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cache", "weft")
	return Config{
		PIDFile:             filepath.Join(cacheDir, "coordinator.pid"),
		LogPath:             filepath.Join(cacheDir, "coordinator-operations.log"),
		PollInterval:        30 * time.Second,
		SyncInterval:        60 * time.Second,
		RemediationInterval: 30 * time.Second,
	}
}

// Coordinator is the central daemon that manages cloud sweeps and host monitoring.
type Coordinator struct {
	db        *sql.DB
	config    Config
	appConfig *config.Config
	hostState *services.HostStateManager
	logger    *log.Logger

	// Composable services
	prober     *services.HostProber
	relay      *services.RelayProcessor
	syncer     *services.HostSyncer
	remediator *services.Remediator
	reconciler *campaign.Reconciler

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

	c := &Coordinator{
		db:         database,
		config:     cfg,
		appConfig:  appCfg,
		hostState:  hostState,
		logger:     logger,
		reconciler: campaign.NewReconciler(),
	}

	c.prober = services.NewHostProber(hostState, cfg.PollInterval)
	c.relay = services.NewRelayProcessor(database, logger, appCfg)
	c.syncer = services.NewHostSyncer(database, hostState, logger, cfg.SyncInterval)
	c.remediator = services.NewRemediator(database, logger, appCfg, cfg.RemediationInterval)

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

	oplog.Log(oplog.OpCoordinatorStart)
	c.logger.Println("coordinator started")

	// Seed host state from inventory so all known hosts are tracked from the start.
	// Run in a goroutine so the event loop starts immediately while probes complete.
	c.hostState.SeedFromInventory()
	go c.prober.ProbeAllParallel()

	// Start composable background services
	go c.prober.Start(ctx)
	go c.relay.Start(ctx)
	go c.syncer.Start(ctx)
	go c.remediator.Start(ctx)

	vastaiSweepTicker := time.NewTicker(60 * time.Second)
	defer vastaiSweepTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			oplog.Log(oplog.OpCoordinatorStop)
			c.logger.Println("coordinator stopped")
			return nil

		case <-vastaiSweepTicker.C:
			r2Client := c.sweepVastaiResults()
			c.reconcileAndNotifyCampaigns(r2Client)
		}
	}
}

// reconcileAndNotifyCampaigns checks for newly-completed campaigns and sends Slack notifications.
func (c *Coordinator) reconcileAndNotifyCampaigns(r2Client *r2.Client) {
	if c.reconciler == nil {
		c.reconciler = campaign.NewReconciler()
	}
	if _, err := c.reconciler.ReconcileLaunches(c.db, c.CloudClients, r2Client); err != nil {
		c.logger.Printf("reconcile cloud instances: %v", err)
	}

	completed, err := campaign.ReconcileCampaigns(c.db)
	if err != nil {
		c.logger.Printf("reconcile campaigns: %v", err)
		return
	}
	for _, camp := range completed {
		slack.SendCampaignNotification(c.db, *camp)
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
