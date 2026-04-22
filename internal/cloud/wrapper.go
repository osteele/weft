package cloud

// AgentJob describes a job for the campaign manifest, used by weft-agent run-campaign.
type AgentJob struct {
	ID         int64           `json:"id"`
	RunID      int64           `json:"run_id,omitempty"`
	Command    string          `json:"cmd"`
	Dir        string          `json:"dir,omitempty"`
	Tags       []string        `json:"tags,omitempty"`
	UsesGPU    bool            `json:"uses_gpu,omitempty"`
	OutputDirs []string        `json:"output_dirs,omitempty"`
	Produces   []string        `json:"produces,omitempty"`
	Needs      []string        `json:"needs,omitempty"`
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

// CampaignManifest is uploaded to R2 by the coordinator and read by weft-agent run-campaign.
type CampaignManifest struct {
	Jobs                []AgentJob        `json:"jobs"`
	SelfDestructCmd     string            `json:"self_destruct_cmd"`
	Env                 map[string]string `json:"env,omitempty"`
	SkipWorkdirDeletion bool              `json:"skip_workdir_deletion,omitempty"`
	GPUWarmup           bool              `json:"gpu_warmup,omitempty"`
	// CostPerHourCents is the whole-instance cost used by the agent to pick
	// hang-watchdog thresholds. Zero = unknown (use conservative thresholds).
	CostPerHourCents int `json:"cost_per_hour_cents,omitempty"`
}
