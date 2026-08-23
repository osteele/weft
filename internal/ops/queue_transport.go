package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/inventoryqueue"
	"github.com/osteele/weft/internal/opsqueue"
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
		return nil, fmt.Errorf("read R2 runner state for %s: %w", host, err)
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
	if state.UpdatedAt.IsZero() || age > inventoryStateMaxAge || age < -inventoryStateMaxAge {
		return nil, fmt.Errorf("R2 runner state for %s is stale (updated %s)", host, state.UpdatedAt.Format(time.RFC3339))
	}
	return &state.Runner, nil
}
