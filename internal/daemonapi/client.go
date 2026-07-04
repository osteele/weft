package daemonapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/osteele/weft/internal/ops"
)

type Watcher struct {
	conn    net.Conn
	decoder *json.Decoder
}

type Subscription struct {
	conn    net.Conn
	decoder *json.Decoder
	Ready   Event
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

func DialSubscribe(ctx context.Context, socketPath string, sub SubscriptionRequest) (*Subscription, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	req := Request{
		Type:      RequestSubscribe,
		ClientPID: os.Getpid(),
		Subscribe: &sub,
	}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send subscribe request: %w", err)
	}
	decoder := json.NewDecoder(conn)
	var ready Event
	if err := decoder.Decode(&ready); err != nil {
		conn.Close()
		return nil, fmt.Errorf("read subscribe ready: %w", err)
	}
	if ready.Type == EventError {
		conn.Close()
		return nil, fmt.Errorf("subscribe %s: %s", sub.Resource, ready.Error)
	}
	if ready.Type != EventSubscriptionReady {
		conn.Close()
		return nil, fmt.Errorf("subscribe %s: unexpected response %q", sub.Resource, ready.Type)
	}
	return &Subscription{conn: conn, decoder: decoder, Ready: ready}, nil
}

func DialSubscribeJobStatus(ctx context.Context, socketPath string, jobIDs []int64, timeout time.Duration) (*Subscription, error) {
	sub := SubscriptionRequest{
		Resource: ResourceJobStatus,
		JobIDs:   append([]int64(nil), jobIDs...),
	}
	if timeout > 0 {
		sub.TimeoutSeconds = int64(timeout.Round(time.Second) / time.Second)
		if sub.TimeoutSeconds == 0 {
			sub.TimeoutSeconds = 1
		}
	}
	return DialSubscribe(ctx, socketPath, sub)
}

func DialSubscribeActivity(ctx context.Context, socketPath string, sub SubscriptionRequest) (*Subscription, error) {
	sub.Resource = ResourceActivity
	return DialSubscribe(ctx, socketPath, sub)
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

func DialSubmitJob(ctx context.Context, socketPath string, params ops.QueueJobParams) (int64, error) {
	event, err := roundTrip(ctx, socketPath, Request{
		Type:      RequestSubmitJob,
		ClientPID: os.Getpid(),
		SubmitJob: &SubmitJobRequest{
			Params: params,
		},
	})
	if err != nil {
		return 0, err
	}
	if event.Type == EventError {
		return 0, fmt.Errorf("submit job: %s", event.Error)
	}
	if event.Type != EventJobSubmitted || event.SubmittedJob == nil {
		return 0, fmt.Errorf("submit job: unexpected response %q", event.Type)
	}
	return event.SubmittedJob.JobID, nil
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

func (s *Subscription) Next() (Event, error) {
	var event Event
	if err := s.decoder.Decode(&event); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (s *Subscription) Close() error {
	return s.conn.Close()
}
