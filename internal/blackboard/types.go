// Package blackboard defines the R2-backed federated job coordination surface.
package blackboard

import "time"

const Version = "v1"

type AutopilotState struct {
	Version     string `json:"version"`
	State       string `json:"state"`
	Runner      string `json:"runner,omitempty"`
	HeartbeatAt string `json:"heartbeat_at,omitempty"`
	ExportedAt  string `json:"exported_at"`
	Message     string `json:"message,omitempty"`
}

type JobSpec struct {
	Version     string   `json:"version"`
	JobID       int64    `json:"job_id"`
	Project     string   `json:"project,omitempty"`
	Command     string   `json:"command"`
	WorkingDir  string   `json:"working_dir,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	GPU         string   `json:"gpu,omitempty"`
	GPUClass    string   `json:"gpu_class,omitempty"`
	GPUMemGB    *int     `json:"gpu_mem_gb,omitempty"`
	Inputs      []string `json:"inputs,omitempty"`
	Needs       []string `json:"needs,omitempty"`
	PublishedAt string   `json:"published_at"`
	DBUpdatedAt int64    `json:"db_updated_at,omitempty"`
}

type Claim struct {
	Version    string `json:"version"`
	JobID      int64  `json:"job_id"`
	AgentID    string `json:"agent_id"`
	LaunchID   int64  `json:"launch_id,omitempty"`
	ClaimKind  string `json:"claim_kind"`
	CreatedAt  string `json:"created_at"`
	RenewedAt  string `json:"renewed_at"`
	ExpiresAt  string `json:"expires_at"`
	AgentPhase string `json:"agent_phase,omitempty"`
}

func (c Claim) Expired(now time.Time) bool {
	expiresAt, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
	return err == nil && !expiresAt.After(now)
}

type Assignment struct {
	Version    string `json:"version"`
	JobID      int64  `json:"job_id"`
	AgentID    string `json:"agent_id"`
	Accepted   bool   `json:"accepted"`
	RunID      int64  `json:"run_id,omitempty"`
	LaunchID   int64  `json:"launch_id,omitempty"`
	AssignedAt string `json:"assigned_at"`
	Message    string `json:"message,omitempty"`
}

type AgentHeartbeat struct {
	Version     string   `json:"version"`
	AgentID     string   `json:"agent_id"`
	LaunchID    int64    `json:"launch_id,omitempty"`
	Host        string   `json:"host,omitempty"`
	Phase       string   `json:"phase,omitempty"`
	CurrentJobs []int64  `json:"current_jobs,omitempty"`
	Capacity    Capacity `json:"capacity,omitempty"`
	HeartbeatAt string   `json:"heartbeat_at"`
}

type Capacity struct {
	FreeSlots int      `json:"free_slots,omitempty"`
	GPUClass  string   `json:"gpu_class,omitempty"`
	GPUMemGB  int      `json:"gpu_mem_gb,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

type Event struct {
	Version   string         `json:"version"`
	EventID   string         `json:"event_id"`
	Kind      string         `json:"kind"`
	JobID     int64          `json:"job_id,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	CreatedAt string         `json:"created_at"`
	Detail    string         `json:"detail,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}
