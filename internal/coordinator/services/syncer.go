package services

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/osteele/weft/internal/ops"
)

// HostSyncer periodically syncs job state from online remote hosts.
type HostSyncer struct {
	db        *sql.DB
	hostState *HostStateManager
	logger    *log.Logger
	interval  time.Duration
	timeout   time.Duration
}

// NewHostSyncer creates a new host syncer service.
func NewHostSyncer(db *sql.DB, hostState *HostStateManager, logger *log.Logger, interval time.Duration) *HostSyncer {
	return &HostSyncer{
		db:        db,
		hostState: hostState,
		logger:    logger,
		interval:  interval,
		timeout:   30 * time.Second,
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
		_, err := ops.SyncHost(s.db, host, ops.HostSyncOptions{
			Timeout: s.timeout,
		}, nil)
		if err != nil {
			s.logger.Printf("sync %s: %v", host, err)
		}
	}
}
