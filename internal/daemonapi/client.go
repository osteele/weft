package daemonapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

type Watcher struct {
	conn    net.Conn
	decoder *json.Decoder
}

func DialWatchJobs(ctx context.Context, socketPath string, jobIDs []int64, timeout time.Duration) (*Watcher, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	req := Request{
		Type:      RequestWatchJobs,
		JobIDs:    jobIDs,
		ClientPID: os.Getpid(),
	}
	if timeout > 0 {
		req.TimeoutSeconds = int64(timeout.Round(time.Second) / time.Second)
		if req.TimeoutSeconds == 0 {
			req.TimeoutSeconds = 1
		}
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send watch request: %w", err)
	}
	return &Watcher{conn: conn, decoder: json.NewDecoder(conn)}, nil
}

func DialDaemonInfo(ctx context.Context, socketPath string) (DaemonInfo, error) {
	event, err := roundTrip(ctx, socketPath, Request{Type: RequestDaemonInfo, ClientPID: os.Getpid()})
	if err != nil {
		return DaemonInfo{}, err
	}
	if event.Type == EventError {
		return DaemonInfo{}, fmt.Errorf("daemon info: %s", event.Error)
	}
	if event.Type != EventDaemonInfo || event.Daemon == nil {
		return DaemonInfo{}, fmt.Errorf("daemon info: unexpected response %q", event.Type)
	}
	return *event.Daemon, nil
}

func DialShutdown(ctx context.Context, socketPath string) error {
	event, err := roundTrip(ctx, socketPath, Request{Type: RequestShutdown, ClientPID: os.Getpid()})
	if err != nil {
		return err
	}
	if event.Type == EventError {
		return fmt.Errorf("daemon shutdown: %s", event.Error)
	}
	if event.Type != EventDone {
		return fmt.Errorf("daemon shutdown: unexpected response %q", event.Type)
	}
	return nil
}

func roundTrip(ctx context.Context, socketPath string, req Request) (Event, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return Event{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Event{}, fmt.Errorf("send daemon request: %w", err)
	}
	var event Event
	if err := json.NewDecoder(conn).Decode(&event); err != nil {
		return Event{}, fmt.Errorf("read daemon response: %w", err)
	}
	return event, nil
}

func (w *Watcher) Next() (Event, error) {
	var event Event
	if err := w.decoder.Decode(&event); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (w *Watcher) Close() error {
	return w.conn.Close()
}
