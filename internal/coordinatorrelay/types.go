package coordinatorrelay

import (
	"time"
)

const (
	OpSubmitJob      = "submit_job"
	OpUpdateQueued   = "update_queued_job"
	OpQueuePriority  = "queue_priority"
	OpKillJob        = "kill_job"
	OpCancelJob      = "cancel_job"
	OpPauseJob       = "pause_job"
	OpResumeJob      = "resume_job"
	OpRequeueJob     = "requeue_job"
	SourceRefNone    = "none"
	SourceRefCache   = "coordinator-cache"
	SourceRefR2      = "r2"
	SourceEntryDir   = "dir"
	SourceEntryFile  = "file"
	DefaultAckWait   = 30 * time.Second
	DefaultPollDelay = 500 * time.Millisecond
)

type Request struct {
	RequestID string            `json:"request_id"`
	CreatedAt string            `json:"created_at"`
	Op        string            `json:"op"`
	JobID     int64             `json:"job_id"`
	Client    *ClientMetadata   `json:"client,omitempty"`
	Submit    *SubmitJobPayload `json:"submit,omitempty"`
	Update    *UpdateJobPayload `json:"update,omitempty"`
	Command   *CommandPayload   `json:"command,omitempty"`
	Source    *SourceBundleRef  `json:"source,omitempty"`
}

type ClientMetadata struct {
	Hostname string `json:"hostname,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Version  string `json:"version,omitempty"`
}

type SubmitJobPayload struct {
	Host        string   `json:"host,omitempty"`
	WorkingDir  string   `json:"working_dir,omitempty"`
	Command     string   `json:"command"`
	Description string   `json:"description,omitempty"`
	Project     string   `json:"project,omitempty"`
	EnvVars     []string `json:"env_vars,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	GPU         string   `json:"gpu,omitempty"`
	GPUClass    string   `json:"gpu_class,omitempty"`
	GPUMemGB    *int     `json:"gpu_mem_gb,omitempty"`
	DepSpec     string   `json:"dep_spec,omitempty"`
	Inputs      []string `json:"inputs,omitempty"`
	Outputs     []string `json:"outputs,omitempty"`
	OutputDirs  []string `json:"output_dirs,omitempty"`
	Produces    []string `json:"produces,omitempty"`
	Needs       []string `json:"needs,omitempty"`
}

type UpdateJobPayload struct {
	WorkingDir   *string  `json:"working_dir,omitempty"`
	Command      *string  `json:"command,omitempty"`
	Description  *string  `json:"description,omitempty"`
	Project      *string  `json:"project,omitempty"`
	EnvVars      []string `json:"env_vars,omitempty"`
	ClearEnv     bool     `json:"clear_env,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	GPU          *string  `json:"gpu,omitempty"`
	GPUClass     *string  `json:"gpu_class,omitempty"`
	GPUMemGB     *int     `json:"gpu_mem_gb,omitempty"`
	CPUAllotment *int     `json:"cpu_allotment,omitempty"`
	DepSpec      *string  `json:"dep_spec,omitempty"`
	Inputs       []string `json:"inputs,omitempty"`
	ClearInputs  bool     `json:"clear_inputs,omitempty"`
	Outputs      []string `json:"outputs,omitempty"`
	OutputDirs   []string `json:"output_dirs,omitempty"`
	Produces     []string `json:"produces,omitempty"`
	Needs        []string `json:"needs,omitempty"`
}

type CommandPayload struct {
	TargetStatus string `json:"target_status,omitempty"`
}

type SourceBundleRef struct {
	Kind    string              `json:"kind"`
	Hash    string              `json:"hash,omitempty"`
	Path    string              `json:"path,omitempty"`
	Entries []SourceBundleEntry `json:"entries,omitempty"`
}

type SourceBundleEntry struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	RelPath    string `json:"rel_path"`
	RemotePath string `json:"remote_path"`
	Delete     bool   `json:"delete,omitempty"`
}

type Ack struct {
	RequestID   string `json:"request_id"`
	ProcessedAt string `json:"processed_at"`
	Accepted    bool   `json:"accepted"`
	JobID       int64  `json:"job_id,omitempty"`
	Host        string `json:"host,omitempty"`
	Message     string `json:"message,omitempty"`
}
