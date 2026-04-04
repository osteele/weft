package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/r2"
)

type GraceCommandKind string

const (
	GraceCommandJobs    GraceCommandKind = "jobs"
	GraceCommandExtend  GraceCommandKind = "extend"
	GraceCommandRelease GraceCommandKind = "release"
)

type GraceCommandAck struct {
	RequestID  string           `json:"request_id"`
	Kind       GraceCommandKind `json:"kind"`
	ReceivedAt string           `json:"received_at"`
	Accepted   bool             `json:"accepted"`
	Message    string           `json:"message,omitempty"`
}

const defaultGraceAckPollInterval = 500 * time.Millisecond

type graceAckPollIntervalKey struct{}

// WithGraceAckPollInterval sets the polling interval used while waiting for
// grace command acknowledgments. Non-positive values are ignored.
func WithGraceAckPollInterval(ctx context.Context, interval time.Duration) context.Context {
	if interval <= 0 {
		return ctx
	}
	return context.WithValue(ctx, graceAckPollIntervalKey{}, interval)
}

type GraceStore interface {
	PutObject(ctx context.Context, key string, body io.Reader, contentType string) error
	GetObject(ctx context.Context, key string) ([]byte, error)
	DeleteObject(ctx context.Context, key string) error
}

// SourceUpdate describes one source snapshot update to apply before running
// jobs received through the grace/reuse control plane.
type SourceUpdate struct {
	RemoteDir string `json:"remote_dir"`
	R2Key     string `json:"r2_key"`
}

// GraceJobsRequest is the typed payload written under grace/<id>/jobs/.
// Sources are applied before jobs are enqueued on the instance.
type GraceJobsRequest struct {
	Jobs    []cloud.AgentJob `json:"jobs"`
	Sources []SourceUpdate   `json:"sources,omitempty"`
}

var ErrControlObjectNotFound = errors.New("control object not found")

func NewGraceRequestID(instanceID int64) string {
	return fmt.Sprintf("%d-%d-%06d", time.Now().UTC().UnixNano(), instanceID, rand.Intn(1000000))
}

func GraceJobsPrefix(instanceID int64) string {
	return fmt.Sprintf("grace/%d/jobs/", instanceID)
}

func GraceJobRequest(instanceID int64, requestID string) string {
	return fmt.Sprintf("%s%s.json", GraceJobsPrefix(instanceID), requestID)
}

func GraceExtendPrefix(instanceID int64) string {
	return fmt.Sprintf("grace/%d/extend/", instanceID)
}

func GraceExtendRequest(instanceID int64, requestID string) string {
	return fmt.Sprintf("%s%s.txt", GraceExtendPrefix(instanceID), requestID)
}

func GraceReleasePrefix(instanceID int64) string {
	return fmt.Sprintf("grace/%d/release/", instanceID)
}

func GraceReleaseRequest(instanceID int64, requestID string) string {
	return fmt.Sprintf("%s%s.txt", GraceReleasePrefix(instanceID), requestID)
}

func GraceCommandAcksPrefix(instanceID int64) string {
	return fmt.Sprintf("grace/%d/acks/", instanceID)
}

func GraceCommandAckKey(instanceID int64, requestID string) string {
	return fmt.Sprintf("%s%s.json", GraceCommandAcksPrefix(instanceID), requestID)
}

func SendGraceJobPayload(ctx context.Context, store GraceStore, instanceID int64, payload GraceJobsRequest) (*GraceCommandAck, error) {
	requestID := NewGraceRequestID(instanceID)
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode jobs request: %w", err)
	}
	if err := store.PutObject(ctx, GraceJobRequest(instanceID, requestID), bytes.NewReader(data), "application/json"); err != nil {
		return nil, fmt.Errorf("write jobs request: %w", err)
	}
	return waitForAcceptedGraceAck(ctx, store, instanceID, requestID)
}

func SendGraceExtend(ctx context.Context, store GraceStore, instanceID int64, duration time.Duration) (*GraceCommandAck, error) {
	requestID := NewGraceRequestID(instanceID)
	if err := store.PutObject(ctx, GraceExtendRequest(instanceID, requestID), bytes.NewReader([]byte(duration.String())), "text/plain"); err != nil {
		return nil, fmt.Errorf("write extend request: %w", err)
	}
	return waitForAcceptedGraceAck(ctx, store, instanceID, requestID)
}

func SendGraceRelease(ctx context.Context, store GraceStore, instanceID int64) (*GraceCommandAck, error) {
	requestID := NewGraceRequestID(instanceID)
	if err := store.PutObject(ctx, GraceReleaseRequest(instanceID, requestID), bytes.NewReader([]byte("release")), "text/plain"); err != nil {
		return nil, fmt.Errorf("write release request: %w", err)
	}
	return waitForAcceptedGraceAck(ctx, store, instanceID, requestID)
}

func WaitForGraceCommandAck(ctx context.Context, store GraceStore, instanceID int64, requestID string) (*GraceCommandAck, error) {
	if requestID == "" {
		return nil, fmt.Errorf("request ID is required")
	}
	pollInterval := defaultGraceAckPollInterval
	if interval, ok := ctx.Value(graceAckPollIntervalKey{}).(time.Duration); ok && interval > 0 {
		pollInterval = interval
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	key := GraceCommandAckKey(instanceID, requestID)
	for {
		data, err := store.GetObject(ctx, key)
		switch {
		case err == nil:
			var ack GraceCommandAck
			if err := json.Unmarshal(data, &ack); err != nil {
				return nil, fmt.Errorf("decode grace ack: %w", err)
			}
			_ = store.DeleteObject(context.Background(), key)
			return &ack, nil
		case isMissingControlObject(err):
		default:
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForAcceptedGraceAck(ctx context.Context, store GraceStore, instanceID int64, requestID string) (*GraceCommandAck, error) {
	ack, err := WaitForGraceCommandAck(ctx, store, instanceID, requestID)
	if err != nil {
		return nil, err
	}
	if ack != nil && !ack.Accepted {
		if ack.Message == "" {
			ack.Message = "command rejected"
		}
		return ack, fmt.Errorf("%s", ack.Message)
	}
	return ack, nil
}

func isMissingControlObject(err error) bool {
	return errors.Is(err, ErrControlObjectNotFound) || r2.IsNotFound(err)
}
