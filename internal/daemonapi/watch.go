package daemonapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/osteele/weft/internal/db"
)

const (
	RequestWatchJobs = "watch_jobs"

	EventSnapshot = "snapshot"
	EventDone     = "done"
	EventError    = "error"
)

type Request struct {
	Type           string  `json:"type"`
	JobIDs         []int64 `json:"job_ids,omitempty"`
	TimeoutSeconds int64   `json:"timeout_seconds,omitempty"`
	ClientPID      int     `json:"client_pid,omitempty"`
	Command        string  `json:"command,omitempty"`
}

type JobSnapshot struct {
	ID              int64  `json:"id"`
	Found           bool   `json:"found"`
	Status          string `json:"status,omitempty"`
	EffectiveStatus string `json:"effective_status,omitempty"`
	Host            string `json:"host,omitempty"`
}

type Event struct {
	Type  string        `json:"type"`
	Jobs  []JobSnapshot `json:"jobs,omitempty"`
	Error string        `json:"error,omitempty"`
}

type Server struct {
	listener   net.Listener
	socketPath string
	closeOnce  sync.Once
}

func StartServer(ctx context.Context, database *sql.DB, socketPath string) (*Server, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("empty daemon watch socket path")
	}
	if err := prepareSocketPath(socketPath); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on daemon watch socket: %w", err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		_ = os.Remove(socketPath)
		return nil, fmt.Errorf("chmod daemon watch socket: %w", err)
	}
	s := &Server{listener: listener, socketPath: socketPath}
	go s.acceptLoop(ctx, database)
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	return s, nil
}

func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.listener.Close()
		_ = os.Remove(s.socketPath)
	})
	return err
}

func prepareSocketPath(socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("create daemon watch socket directory: %w", err)
	}
	if _, err := os.Lstat(socketPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat daemon watch socket: %w", err)
	}
	conn, err := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		return fmt.Errorf("daemon watch socket already in use: %s", socketPath)
	}
	if err := os.Remove(socketPath); err != nil {
		return fmt.Errorf("remove stale daemon watch socket: %w", err)
	}
	return nil
}

func (s *Server) acceptLoop(ctx context.Context, database *sql.DB) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go handleConn(ctx, database, conn)
	}
}

func handleConn(ctx context.Context, database *sql.DB, conn net.Conn) {
	defer conn.Close()
	var req Request
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)
	if err := decoder.Decode(&req); err != nil {
		_ = encoder.Encode(Event{Type: EventError, Error: err.Error()})
		return
	}
	switch req.Type {
	case RequestWatchJobs:
		watchJobs(ctx, database, encoder, req)
	default:
		_ = encoder.Encode(Event{Type: EventError, Error: fmt.Sprintf("unsupported request type %q", req.Type)})
	}
}

func watchJobs(parent context.Context, database *sql.DB, encoder *json.Encoder, req Request) {
	if len(req.JobIDs) == 0 {
		_ = encoder.Encode(Event{Type: EventError, Error: "watch_jobs requires at least one job id"})
		return
	}
	ctx := parent
	cancel := func() {}
	if req.TimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(req.TimeoutSeconds)*time.Second)
	}
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	lastKey := ""
	for {
		snapshots, allDone, err := jobSnapshots(database, req.JobIDs)
		if err != nil {
			_ = encoder.Encode(Event{Type: EventError, Error: err.Error()})
			return
		}
		key := snapshotsKey(snapshots)
		if key != lastKey {
			lastKey = key
			if err := encoder.Encode(Event{Type: EventSnapshot, Jobs: snapshots}); err != nil {
				return
			}
		}
		if allDone {
			_ = encoder.Encode(Event{Type: EventDone, Jobs: snapshots})
			return
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				_ = encoder.Encode(Event{Type: EventError, Error: ctx.Err().Error()})
			}
			return
		case <-ticker.C:
		}
	}
}

func jobSnapshots(database *sql.DB, jobIDs []int64) ([]JobSnapshot, bool, error) {
	jobs, err := db.GetJobsByIDs(database, jobIDs)
	if err != nil {
		return nil, false, err
	}
	snapshots := make([]JobSnapshot, 0, len(jobIDs))
	allDone := true
	for _, id := range jobIDs {
		job := jobs[id]
		if job == nil {
			snapshots = append(snapshots, JobSnapshot{ID: id, Found: false})
			continue
		}
		effective := job.EffectiveStatus()
		snapshots = append(snapshots, JobSnapshot{
			ID:              id,
			Found:           true,
			Status:          job.Status,
			EffectiveStatus: effective,
			Host:            job.Host,
		})
		if !isWaitTerminalStatus(effective) {
			allDone = false
		}
	}
	return snapshots, allDone, nil
}

func isWaitTerminalStatus(s string) bool {
	switch s {
	case db.StatusCompleted, db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
		return true
	default:
		return false
	}
}

func snapshotsKey(snapshots []JobSnapshot) string {
	data, err := json.Marshal(snapshots)
	if err != nil {
		return ""
	}
	return string(data)
}
