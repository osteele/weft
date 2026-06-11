package r2upload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// DefaultMarkerTimeout is the hard ceiling on WriteFailureMarker's own R2
// PUT, intentionally short so a wedged R2 doesn't make us recurse into
// another long-running upload right before self-destruct.
const DefaultMarkerTimeout = 10 * time.Second

// FailureMarker is the JSON written to R2 when a drain ends with
// status != ok. Field order mirrors Result so a downstream reader can decode
// either shape interchangeably.
type FailureMarker struct {
	Status              string  `json:"status"`
	Reason              string  `json:"reason,omitempty"`
	KilledBy            string  `json:"killed_by,omitempty"`
	BytesUploaded       int64   `json:"bytes_uploaded"`
	BytesTotal          int64   `json:"bytes_total"`
	ElapsedSeconds      float64 `json:"elapsed_seconds"`
	StartedAtUnix       int64   `json:"started_at_unix"`
	EndedAtUnix         int64   `json:"ended_at_unix"`
	CeilingSeconds      float64 `json:"ceiling_seconds"`
	StallTimeoutSeconds float64 `json:"stall_timeout_seconds"`
	// Kind preserves the stall discrimination from [Result.StallKind]
	// (never_started / mid_transfer / heartbeat / slow_pace) so displays can
	// say which kind of stall fired. Empty for non-stall failures and for
	// markers written by older agents, which display as a generic stall.
	Kind       StallKind `json:"stall_kind,omitempty"`
	StderrTail string    `json:"stderr_tail,omitempty"`

	// Context fields the caller fills in. These describe what the failed
	// drain was trying to upload — useful for the CLI display layer.
	JobID      int64  `json:"job_id,omitempty"`
	RunID      int64  `json:"run_id,omitempty"`
	InstanceID int64  `json:"instance_id,omitempty"`
	SourcePath string `json:"source_path,omitempty"`
	DestRemote string `json:"dest_remote,omitempty"`
}

// NewFailureMarker copies the salient fields from a Result.
func NewFailureMarker(r Result) FailureMarker {
	return FailureMarker{
		Status:              r.Status,
		Reason:              r.Reason,
		KilledBy:            r.KilledBy,
		BytesUploaded:       r.BytesUploaded,
		BytesTotal:          r.BytesTotal,
		ElapsedSeconds:      r.ElapsedSeconds,
		StartedAtUnix:       r.StartedAtUnix,
		EndedAtUnix:         r.EndedAtUnix,
		CeilingSeconds:      r.CeilingSeconds,
		StallTimeoutSeconds: r.StallTimeoutSeconds,
		Kind:                r.StallKind,
		StderrTail:          r.StderrTail,
	}
}

// Cause renders the marker's failure discriminator for display.
// When the marker carries a stall kind (never_started / mid_transfer /
// heartbeat / slow_pace) it is surfaced alongside the killed-by token, e.g.
// "stall: mid_transfer". Markers written by older agents have no kind and
// fall back to the generic killed-by token ("stall").
func (m FailureMarker) Cause() string {
	if m.Kind == "" || string(m.Kind) == m.KilledBy {
		return m.KilledBy
	}
	return fmt.Sprintf("%s: %s", m.KilledBy, m.Kind)
}

// MarkerWriter abstracts the R2 PUT for tests. The agent's wrapper provides
// the real implementation that shells out to rclone rcat.
type MarkerWriter interface {
	Put(ctx context.Context, key string, body io.Reader, contentType string) error
}

// WriteFailureMarker uploads a FailureMarker JSON to R2 with a bounded
// timeout. It is best-effort: a non-nil return is logged by the caller but
// must not block self-destruct. The timeout is fixed (not size-derived) so
// this path can never enter the watchdog cycle.
func WriteFailureMarker(ctx context.Context, w MarkerWriter, key string, marker FailureMarker, timeout time.Duration) error {
	if w == nil {
		return errors.New("nil marker writer")
	}
	if timeout <= 0 {
		timeout = DefaultMarkerTimeout
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("marshal failure marker: %w", err)
	}
	putCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return w.Put(putCtx, key, bytes.NewReader(data), "application/json")
}

// silence unused-import linting if io ever drops away
var _ io.Reader = (*bytes.Reader)(nil)
