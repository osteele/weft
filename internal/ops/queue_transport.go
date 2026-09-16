package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventoryqueue"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/util"
)

const (
	queueTransportSSH    = "ssh"
	queueTransportR2Pull = "r2_pull"
	inventoryStateMaxAge = 30 * time.Second
)

type inventoryQueueStore interface {
	GetObject(context.Context, string) ([]byte, error)
	PutObject(context.Context, string, io.Reader, string) error
}

var (
	queueRequestSequence   atomic.Uint64
	loadQueueConfig        = config.Load
	newInventoryQueueStore = func() (inventoryQueueStore, error) {
		return defaultR2Client()
	}
	appendQueueCommandSSH = opsqueue.AppendCommand
)

type exactQueueWriterEvidence uint8

const (
	exactQueueWriterNotApplicable exactQueueWriterEvidence = iota
	exactQueueWriterUnknown
	exactQueueWriterAbsent
	exactQueueWriterActive
)

var fetchR2RunnerStateForWriter = fetchR2RunnerState

func queueTransportForHost(host string) (string, error) {
	cfg, err := loadQueueConfig()
	if err != nil {
		return "", fmt.Errorf("load queue transport config: %w", err)
	}
	transport := cfg.HostQueueTransport(host)
	if transport == "" {
		transport = queueTransportSSH
	}
	switch transport {
	case queueTransportSSH, queueTransportR2Pull:
		return transport, nil
	default:
		return "", fmt.Errorf("host %s has unsupported queue_transport %q", host, transport)
	}
}

func hostUsesR2Queue(host string) bool {
	transport, err := queueTransportForHost(host)
	return err == nil && transport == queueTransportR2Pull
}

// HostUsesR2Queue reports whether host dispatches queue commands over the
// R2 inventory transport. Only such hosts publish the state envelope that
// carries the running agent's version.
func HostUsesR2Queue(host string) bool {
	return hostUsesR2Queue(host)
}

// R2StateView is a published state envelope for a display surface.
// fetchR2RunnerState gates placement decisions on freshness and discards
// stale envelopes; status instead degrades to stale-with-age, because the
// envelope's age is itself evidence about the runner that published it.
type R2StateView struct {
	// State is the decoded envelope; nil only when Absent is true.
	State *inventoryqueue.State
	// Age is time since the envelope claims publication.
	Age time.Duration
	// Stale reports whether Age exceeds the freshness bound that
	// placement decisions require.
	Stale bool
	// Absent reports confirmed absence: no state has been published for
	// this host.
	Absent bool
}

// FetchR2StateView reads the host's published state envelope. A non-nil
// error with a nil view means the envelope could not be read as a valid one
// for this host (R2 unavailable, decode failure, version or host mismatch);
// callers must treat that as unknown, never as the runner being down or the
// version being absent.
func FetchR2StateView(host string) (*R2StateView, error) {
	store, err := newInventoryQueueStore()
	if err != nil {
		return nil, fmt.Errorf("create R2 inventory queue client: %w", err)
	}
	key, err := inventoryqueue.StateKey(host)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	data, err := store.GetObject(ctx, key)
	if err != nil {
		if r2.IsNotFound(err) {
			return &R2StateView{Absent: true}, nil
		}
		return nil, fmt.Errorf("read R2 runner state for %s: %w", host, err)
	}
	if len(data) == 0 {
		return &R2StateView{Absent: true}, nil
	}
	var state inventoryqueue.State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode R2 runner state for %s: %w", host, err)
	}
	if state.Version != inventoryqueue.Version {
		return nil, fmt.Errorf("R2 runner state for %s has unsupported version %q", host, state.Version)
	}
	if state.Host != host {
		return nil, fmt.Errorf("R2 runner state for %s is addressed to %q", host, state.Host)
	}
	age := time.Since(state.UpdatedAt)
	stale := state.UpdatedAt.IsZero() || age > inventoryStateMaxAge || age < -inventoryStateMaxAge
	return &R2StateView{State: &state, Age: age, Stale: stale}, nil
}

func appendQueueCommand(host string, command opsqueue.QueueCommand, opts opsqueue.AppendCommandOptions) error {
	transport, err := queueTransportForHost(host)
	if err != nil {
		return err
	}
	if transport == queueTransportSSH {
		return appendQueueCommandSSH(host, command, opts)
	}
	store, err := newInventoryQueueStore()
	if err != nil {
		return fmt.Errorf("create R2 inventory queue client: %w", err)
	}
	now := time.Now().UTC()
	requestID := fmt.Sprintf("%d-%d", now.UnixNano(), queueRequestSequence.Add(1))
	request := inventoryqueue.Request{
		Version: inventoryqueue.Version, RequestID: requestID, Host: host,
		CreatedAt: now, Command: command,
	}
	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("marshal R2 inventory queue request: %w", err)
	}
	key, err := inventoryqueue.RequestKey(host, requestID)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	if err := store.PutObject(ctx, key, bytes.NewReader(data), "application/json"); err != nil {
		return fmt.Errorf("publish R2 inventory queue request: %w", err)
	}
	return nil
}

func fetchR2RunnerState(host string) (*opsqueue.RunnerState, error) {
	view, err := FetchR2StateView(host)
	if err != nil {
		return nil, err
	}
	if view.Absent {
		return nil, fmt.Errorf("read R2 runner state for %s: no state published", host)
	}
	if view.Stale {
		return nil, fmt.Errorf("R2 runner state for %s is stale (updated %s)", host, util.FormatCLITime(view.State.UpdatedAt, "2006-01-02 15:04:05"))
	}
	state := &view.State.Runner
	state.AgentVersion = view.State.AgentVersion
	state.UpdatedAt = view.State.UpdatedAt.Unix()
	return state, nil
}

// observeExactQueueWriter checks whether an R2 inventory runner still owns the
// attempt that a start-now handoff tentatively assigned to a tmux session.
// The runner's run ID is the ownership fence: an older writer must not affect
// the current attempt, while a missing or stale state snapshot is unknown.
func observeExactQueueWriter(job *db.Job) exactQueueWriterEvidence {
	if job == nil || job.UsesQueueRunner() || job.LatestRunID == nil || *job.LatestRunID <= 0 {
		return exactQueueWriterNotApplicable
	}
	pendingStart := job.PendingStatus != nil && *job.PendingStatus == db.StatusRunning
	inventoryExecution := job.Metadata != nil &&
		job.Metadata.Source != nil &&
		job.Metadata.Source.Execution != nil &&
		job.Metadata.Source.Execution.DispatchMode == "pinned_inventory_manifest"
	if job.LastSyncedStatus != db.StatusQueued && !pendingStart && !inventoryExecution {
		return exactQueueWriterNotApplicable
	}
	if !hostUsesR2Queue(job.Host) {
		return exactQueueWriterNotApplicable
	}

	state, err := fetchR2RunnerStateForWriter(job.Host)
	if err != nil || state == nil {
		return exactQueueWriterUnknown
	}
	jobIDText := strconv.FormatInt(job.ID, 10)
	running, ok := state.Running[jobIDText]
	if !ok {
		if state.Current != nil && *state.Current == job.ID {
			return exactQueueWriterUnknown
		}
		for _, pendingJobID := range state.Pending {
			if pendingJobID == job.ID {
				return exactQueueWriterUnknown
			}
		}
		return exactQueueWriterAbsent
	}
	if running.RunID <= 0 {
		return exactQueueWriterUnknown
	}
	if running.RunID == *job.LatestRunID {
		return exactQueueWriterActive
	}
	return exactQueueWriterAbsent
}
