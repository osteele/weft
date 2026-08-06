package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/r2"
)

type GraceCommandKind string

const (
	GraceCommandJobs           GraceCommandKind = "jobs"
	GraceCommandExtend         GraceCommandKind = "extend"
	GraceCommandRelease        GraceCommandKind = "release"
	GraceCommandCancelAttempts GraceCommandKind = "cancel_attempts"
)

// GraceCancelAttemptsRequest tells the agent on `instanceID` to skip any
// pending or running job whose attempt (run) id is in AttemptIDs. Used by
// the move flow after a transfer-claim so the source instance drops the
// superseded attempt rather than running it. See specs/job-move.allium
// "AttemptCancelMarker".
type GraceCancelAttemptsRequest struct {
	AttemptIDs []int64 `json:"attempt_ids"`
}

type GraceCommandAck struct {
	RequestID  string           `json:"request_id"`
	Kind       GraceCommandKind `json:"kind"`
	ReceivedAt string           `json:"received_at"`
	Accepted   bool             `json:"accepted"`
	Message    string           `json:"message,omitempty"`
}

const defaultGraceAckPollInterval = 500 * time.Millisecond

// defaultGraceAckTimeout is how long submit-to-instance / extend / release
// will wait for the instance's agent to ack a grace command before
// returning an error. Slow providers (runpod under load, vast.ai instances
// in grace state with R2 churn) routinely take 20-40s to respond, so the
// default is set with headroom. Override per-call with WithGraceAckTimeout
// or globally with the WEFT_GRACE_ACK_TIMEOUT env var (e.g. "90s").
var defaultGraceAckTimeout = func() time.Duration {
	if v := os.Getenv("WEFT_GRACE_ACK_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 90 * time.Second
}()

type graceAckTimeoutKey struct{}

// WithGraceAckTimeout overrides the default ack wait timeout.
func WithGraceAckTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if timeout <= 0 {
		return ctx
	}
	return context.WithValue(ctx, graceAckTimeoutKey{}, timeout)
}

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
	RemoteDir string       `json:"remote_dir"`
	R2Key     string       `json:"r2_key"`
	Blobs     []SourceBlob `json:"blobs,omitempty"`
}

// SourceBlob describes a content-addressed file materialized relative to a
// source update's RemoteDir.
type SourceBlob = dataplane.SourceBlob

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

func GraceCancelAttemptsPrefix(instanceID int64) string {
	return fmt.Sprintf("grace/%d/cancel_attempts/", instanceID)
}

func GraceCancelAttemptsRequestKey(instanceID int64, requestID string) string {
	return fmt.Sprintf("%s%s.json", GraceCancelAttemptsPrefix(instanceID), requestID)
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

// SendGraceJobPayloadNoAck writes the job request to R2 without waiting for
// an acknowledgment. Use this for running instances where the agent won't
// ack until the current job finishes. The returned request id can be
// reconciled later via CheckGraceCommandAck.
func SendGraceJobPayloadNoAck(ctx context.Context, store GraceStore, instanceID int64, payload GraceJobsRequest) (string, error) {
	requestID := NewGraceRequestID(instanceID)
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode jobs request: %w", err)
	}
	if err := store.PutObject(ctx, GraceJobRequest(instanceID, requestID), bytes.NewReader(data), "application/json"); err != nil {
		return "", fmt.Errorf("write jobs request: %w", err)
	}
	return requestID, nil
}

// SendGraceCancelAttempts writes a cancel-attempts request to R2 (no-ack:
// the source agent processes it between jobs). Empty AttemptIDs is a no-op.
func SendGraceCancelAttempts(ctx context.Context, store GraceStore, instanceID int64, attemptIDs []int64) error {
	if len(attemptIDs) == 0 {
		return nil
	}
	requestID := NewGraceRequestID(instanceID)
	data, err := json.Marshal(GraceCancelAttemptsRequest{AttemptIDs: attemptIDs})
	if err != nil {
		return fmt.Errorf("encode cancel-attempts request: %w", err)
	}
	if err := store.PutObject(ctx, GraceCancelAttemptsRequestKey(instanceID, requestID), bytes.NewReader(data), "application/json"); err != nil {
		return fmt.Errorf("write cancel-attempts request: %w", err)
	}
	return nil
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
	timeout := defaultGraceAckTimeout
	if t, ok := ctx.Value(graceAckTimeoutKey{}).(time.Duration); ok && t > 0 {
		timeout = t
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

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
			return nil, fmt.Errorf("grace ack timeout after %s (instance may be unresponsive)", timeout)
		case <-ticker.C:
		}
	}
}

func CheckGraceCommandAck(ctx context.Context, store GraceStore, instanceID int64, requestID string) (*GraceCommandAck, bool, error) {
	if requestID == "" {
		return nil, false, fmt.Errorf("request ID is required")
	}
	key := GraceCommandAckKey(instanceID, requestID)
	data, err := store.GetObject(ctx, key)
	if err != nil {
		if isMissingControlObject(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var ack GraceCommandAck
	if err := json.Unmarshal(data, &ack); err != nil {
		return nil, true, fmt.Errorf("decode grace ack: %w", err)
	}
	_ = store.DeleteObject(context.Background(), key)
	return &ack, true, nil
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
