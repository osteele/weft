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
