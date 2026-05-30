package cloud

// AgentJob describes a job for the campaign manifest, used by weft-agent run-instance.
type AgentJob struct {
	ID         int64    `json:"id"`
	RunID      int64    `json:"run_id,omitempty"`
	Command    string   `json:"cmd"`
	Dir        string   `json:"dir,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Priority   int      `json:"priority,omitempty"`
	UsesGPU    bool     `json:"uses_gpu,omitempty"`
	OutputDirs []string `json:"output_dirs,omitempty"`
	Produces   []string `json:"produces,omitempty"`
	Needs      []string `json:"needs,omitempty"`
	// Inputs are the declared input refs (e.g. "hf:Qwen/Qwen2.5-7B").
	// Used by the agent to identify which HF cache entries belong to the
	// current workload vs. stale assets from prior reuse.
	Inputs     []string        `json:"inputs,omitempty"`
	CloudNeeds []CloudNeed     `json:"cloud_needs,omitempty"`
	CloudAfter []CloudAfterRef `json:"cloud_after,omitempty"`
	Env        []string        `json:"env,omitempty"`
}

// CloudNeed is a resolved cloud artifact dependency for an agent job.
// R2Key points to the exact object in R2 that should be copied to Path.
type CloudNeed struct {
	Spec  string `json:"spec,omitempty"`
	Path  string `json:"path"`
	R2Key string `json:"r2_key"`
}

// CloudAfterRef identifies a producer job whose success the consumer depends on,
// evaluated by the agent when both jobs run on the same rental instance.
// If the producer ran on this instance and failed (and AllowFailure is false),
// the agent skips the consumer.
type CloudAfterRef struct {
	JobID        int64 `json:"job_id"`
	RunID        int64 `json:"run_id,omitempty"`
	AllowFailure bool  `json:"allow_failure,omitempty"`
}

// CampaignManifest is uploaded to R2 by the coordinator and read by weft-agent run-instance.
type CampaignManifest struct {
	Jobs                []AgentJob        `json:"jobs"`
	SelfDestructCmd     string            `json:"self_destruct_cmd"`
	Env                 map[string]string `json:"env,omitempty"`
	SkipWorkdirDeletion bool              `json:"skip_workdir_deletion,omitempty"`
	GPUWarmup           bool              `json:"gpu_warmup,omitempty"`
	// CostPerHourCents is the whole-instance cost used by the agent to pick
	// hang-watchdog thresholds. Zero = unknown (use conservative thresholds).
	CostPerHourCents int `json:"cost_per_hour_cents,omitempty"`
	// Provider and InstanceType are passed through so the agent can expose
	// them to job processes via WEFT_PROVIDER / WEFT_INSTANCE_TYPE.
	Provider     string `json:"provider,omitempty"`
	InstanceType string `json:"instance_type,omitempty"`
	// RequestedDiskGB is the container disk weft asked the provider to
	// allocate. The agent probes the actual mounted disk at startup and
	// fails fast (infra_failure) if the provider silently delivered
	// substantially less.
	RequestedDiskGB int `json:"requested_disk_gb,omitempty"`
	// Drain holds the upload-drain gate tunables. Zero fields fall back
	// to the r2upload package defaults.
	Drain DrainSettings `json:"drain,omitempty"`
}

// DrainSettings mirrors the user-facing config.CloudDrainConfig in a form
// that travels in the campaign manifest. Seconds-valued ints (rather than
// time.Duration) keep the JSON cleanly readable for humans inspecting the
// manifest in R2.
type DrainSettings struct {
	StallTimeoutSeconds        int   `json:"stall_timeout_seconds,omitempty"`
	InitialStallTimeoutSeconds int   `json:"initial_stall_timeout_seconds,omitempty"`
	HeartbeatTimeoutSeconds    int   `json:"heartbeat_timeout_seconds,omitempty"`
	FloorThroughputBytesPerSec int64 `json:"floor_throughput_bytes_per_sec,omitempty"`
	MaxDrainSeconds            int   `json:"max_drain_seconds,omitempty"`
	BaselineSeconds            int   `json:"baseline_seconds,omitempty"`
	MarkerTimeoutSeconds       int   `json:"marker_timeout_seconds,omitempty"`
	// PaceCheckAfterSeconds: warmup before slow-pace gate activates. Negative
	// disables. Zero → r2upload default.
	PaceCheckAfterSeconds int `json:"pace_check_after_seconds,omitempty"`
	// MinThroughputFraction: required fraction of FloorThroughput (avg over
	// drain lifetime) after warmup. Negative disables. Zero → r2upload default.
	MinThroughputFraction float64 `json:"min_throughput_fraction,omitempty"`
}
