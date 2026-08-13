package instanceintent

// State is an explicit lifecycle phase for a TerminationIntent. Older
// markers (pre-State field) deserialize with state == "" and are treated
// by EffectiveState as "open" or "destroying" depending on whether
// destroy has started.
type State string

const (
	StateOpen       State = "open"       // intent recorded; destroy not yet attempted
	StateDestroying State = "destroying" // provider destroy in progress
	StateSucceeded  State = "succeeded"  // provider confirms instance gone
	StateFailed     State = "failed"     // give-up state after retries exhausted
)

// Marker is the persisted shape of a TerminationIntent for a cloud
// instance: an explicit "this instance is on the way out" record. Stored
// as JSON in launches.termination_intent. Created when the orchestrator
// decides to terminate (cost limit, bootstrap timeout, user terminate,
// grace release, etc.); resolved when the provider confirms the
// instance is gone.
//
// State and the destroy timestamps are partly redundant: State is
// authoritative for the lifecycle phase; the timestamps are kept for
// audit / observability and remain populated by the reconciler.
type Marker struct {
	TerminalStatus    string `json:"terminal_status"`
	TerminationReason string `json:"termination_reason,omitempty"`
	// ProviderInstanceID is populated when provider creation succeeded but
	// the launch's ordinary provider-ID column could not be written. It makes
	// the write-ahead intent independently sufficient for crash recovery.
	ProviderInstanceID     string `json:"provider_instance_id,omitempty"`
	Phase                  string `json:"phase,omitempty"`
	JobID                  int64  `json:"job_id,omitempty"`
	State                  State  `json:"state,omitempty"`
	RequestedAtUnix        int64  `json:"requested_at_unix"`
	DestroyStartedAtUnix   int64  `json:"destroy_started_at_unix,omitempty"`
	DestroySucceededAtUnix int64  `json:"destroy_succeeded_at_unix,omitempty"`
	LastAttemptAtUnix      int64  `json:"last_attempt_at_unix,omitempty"`
	DestroyAttempts        int    `json:"destroy_attempts,omitempty"`
	LastError              string `json:"last_error,omitempty"`
}

// EffectiveState returns the marker's lifecycle phase. Backwards-
// compatible with markers written before the State field: if State is
// empty, it is derived from the destroy timestamps so HasActiveIntent
// continues to work on existing rows.
func (m *Marker) EffectiveState() State {
	if m == nil {
		return ""
	}
	if m.State != "" {
		return m.State
	}
	switch {
	case m.DestroySucceededAtUnix != 0:
		return StateSucceeded
	case m.DestroyStartedAtUnix != 0:
		return StateDestroying
	case m.TerminalStatus != "":
		return StateOpen
	default:
		return ""
	}
}

// IsActive reports whether the intent is still in flight (not yet
// confirmed succeeded or failed-terminal).
func (m *Marker) IsActive() bool {
	if m == nil {
		return false
	}
	switch m.EffectiveState() {
	case StateOpen, StateDestroying:
		return true
	default:
		return false
	}
}
