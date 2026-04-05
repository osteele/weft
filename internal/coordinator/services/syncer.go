package services

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
)

// HostSyncer periodically syncs job state from online remote hosts.
type HostSyncer struct {
	db        *sql.DB
	hostState *HostStateManager
	logger    *slog.Logger
	interval  time.Duration
	timeout   time.Duration
	owner     string
}

// NewHostSyncer creates a new host syncer service.
func NewHostSyncer(db *sql.DB, hostState *HostStateManager, logger *slog.Logger, interval time.Duration) *HostSyncer {
	return &HostSyncer{
		db:        db,
		hostState: hostState,
		logger:    logger,
		interval:  interval,
		timeout:   30 * time.Second,
		owner:     "coordinator-syncer",
	}
}

// Start runs the syncer loop until the context is cancelled.
func (s *HostSyncer) Start(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SyncAll()
		}
	}
}

// SyncAll syncs job state from all online hosts.
func (s *HostSyncer) SyncAll() {
	if s.db == nil {
		return
	}

	onlineHosts := s.hostState.OnlineHosts()
	for _, host := range onlineHosts {
		scope := "sync:host:fast:" + host
		ok, err := db.AcquireAutoLease(s.db, scope, s.owner, 30*time.Second)
		if err != nil {
			s.logger.Debug("sync lease failed", "host", host, "error", err)
			continue
		}
		if !ok {
			continue
		}
		_, err = ops.SyncHost(s.db, host, ops.HostSyncOptions{
			Timeout: s.timeout,
			Mode:    ops.SyncModeStatus,
			Logger:  ops.NewQuietSyncLogger(),
		}, nil)
		_ = db.ReleaseAutoLease(s.db, scope, s.owner)
		if err != nil {
			s.logger.Warn("sync failed", "host", host, "error", err)
		}
	}
}
