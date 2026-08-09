package cloud

import "github.com/osteele/weft/internal/dataplane"

// AgentJob describes a job for the campaign manifest, used by weft-agent run-instance.
type AgentJob struct {
	ID           int64    `json:"id"`
	RunID        int64    `json:"run_id,omitempty"`
	Command      string   `json:"cmd"`
	Dir          string   `json:"dir,omitempty"`
	SlotGPU      bool     `json:"slot_gpu,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Priority     int      `json:"priority,omitempty"`
	UsesGPU      bool     `json:"uses_gpu,omitempty"`
	GPU          string   `json:"gpu,omitempty"`
	GPUClass     string   `json:"gpu_class,omitempty"`
	GPUCount     int      `json:"gpu_count,omitempty"`
	GPUMemGB     int      `json:"gpu_mem_gb,omitempty"`
	Interconnect string   `json:"interconnect,omitempty"`
	CPUCores     int      `json:"cpu_cores,omitempty"`
	OutputDirs   []string `json:"output_dirs,omitempty"`
	Outputs      []string `json:"outputs,omitempty"`
	Produces     []string `json:"produces,omitempty"`
	Needs        []string `json:"needs,omitempty"`
	// Inputs are the declared input refs (e.g. "hf:Qwen/Qwen2.5-7B").
	// Used by the agent to identify which HF cache entries belong to the
	// current workload vs. stale assets from prior reuse.
	Inputs []string `json:"inputs,omitempty"`
	// BestEffortInputs are a subset of Inputs inferred only from source/command
	// auto-detection. The agent stages them opportunistically.
	BestEffortInputs []string        `json:"best_effort_inputs,omitempty"`
	CloudNeeds       []CloudNeed     `json:"cloud_needs,omitempty"`
	CloudAfter       []CloudAfterRef `json:"cloud_after,omitempty"`
	// RestagedOutputs is true when Weft restored this job's previous attempt
	// outputs from R2 before starting the command on a fresh instance.
	RestagedOutputs bool          `json:"restaged_outputs,omitempty"`
	Env             []string      `json:"env,omitempty"`
	SourceMounts    []SourceMount `json:"source_mounts,omitempty"`
}

// SourceMount describes one content-addressed source root mounted on a rental.
type SourceMount struct {
	R2Key         string                 `json:"r2_key"`
	RemoteDir     string                 `json:"remote_dir"`
	LocalDir      string                 `json:"local_dir,omitempty"`
	MountBasename string                 `json:"mount_basename,omitempty"`
	Hash          string                 `json:"hash,omitempty"`
	Blobs         []dataplane.SourceBlob `json:"blobs,omitempty"`
}

// CloudNeed is a resolved cloud artifact dependency for an agent job.
// R2Key points to the exact object in R2 that should be copied to Path.
type CloudNeed struct {
	Spec        string `json:"spec,omitempty"`
	Path        string `json:"path"`
	R2Key       string `json:"r2_key"`
	ContentType string `json:"content_type,omitempty"`
}

// CloudAfterRef identifies a same-instance --needs producer attempt, evaluated
// by the agent when both jobs run on the same rental instance. The submit-time
// db.JobDependencyRef CloudAfter metadata is a separate --after/--after-any
// ordering concept. AllowFailure is reserved on this wire type for manifest
// compatibility; current same-instance --needs classification does not set it.
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
	// RequiredDriverMajor is the minimum NVIDIA driver major version that
	// the wheels installed by this campaign's jobs need at runtime, derived
	// from the placement-time MinDriverVersion. The agent queries
	// nvidia-smi at startup and fails fast (infra_failure) when the host's
	// driver is below this. Defense in depth: at offer-search time some
	// providers don't expose driver_version (RunPod) so we may have landed
	// on an old-driver host even with the cuda/driver floors applied. Zero
	// means no check (job has no driver floor).
	RequiredDriverMajor int `json:"required_driver_major,omitempty"`
	// Drain holds the upload-drain gate tunables. Zero fields fall back
	// to the r2upload package defaults.
	Drain DrainSettings `json:"drain,omitempty"`
	// Publication bounds deferred post-job work on the agent. Zero fields use
	// the agent defaults.
	Publication PublicationSettings `json:"publication,omitempty"`
}

type PublicationSettings struct {
	Workers          int   `json:"workers,omitempty"`
	QueueCapacity    int   `json:"queue_capacity,omitempty"`
	MaxRetainedBytes int64 `json:"max_retained_bytes,omitempty"`
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
