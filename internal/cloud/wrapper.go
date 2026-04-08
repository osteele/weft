package cloud

// AgentJob describes a job for the campaign manifest, used by weft-agent run-campaign.
type AgentJob struct {
	ID         int64    `json:"id"`
	RunID      int64    `json:"run_id,omitempty"`
	Command    string   `json:"cmd"`
	Dir        string   `json:"dir,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	UsesGPU    bool     `json:"uses_gpu,omitempty"`
	OutputDirs []string `json:"output_dirs,omitempty"`
	Produces   []string `json:"produces,omitempty"`
	Needs      []string `json:"needs,omitempty"`
	Env        []string `json:"env,omitempty"`
}

// CampaignManifest is uploaded to R2 by the coordinator and read by weft-agent run-campaign.
type CampaignManifest struct {
	Jobs                []AgentJob        `json:"jobs"`
	SelfDestructCmd     string            `json:"self_destruct_cmd"`
	Env                 map[string]string `json:"env,omitempty"`
	SkipWorkdirDeletion bool              `json:"skip_workdir_deletion,omitempty"`
	GPUWarmup           bool              `json:"gpu_warmup,omitempty"`
}
