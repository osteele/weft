package instanceintent

type Marker struct {
	TerminalStatus         string `json:"terminal_status"`
	TerminationReason      string `json:"termination_reason,omitempty"`
	Phase                  string `json:"phase,omitempty"`
	JobID                  int64  `json:"job_id,omitempty"`
	RequestedAtUnix        int64  `json:"requested_at_unix"`
	DestroyStartedAtUnix   int64  `json:"destroy_started_at_unix,omitempty"`
	DestroySucceededAtUnix int64  `json:"destroy_succeeded_at_unix,omitempty"`
	LastAttemptAtUnix      int64  `json:"last_attempt_at_unix,omitempty"`
	DestroyAttempts        int    `json:"destroy_attempts,omitempty"`
	LastError              string `json:"last_error,omitempty"`
}
