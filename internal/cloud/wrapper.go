package cloud

import "encoding/json"

// AgentJob describes a job for the campaign manifest, used by weft-agent run-campaign.
type AgentJob struct {
	ID      int64  `json:"id"`
	RunID   int64  `json:"run_id,omitempty"`
	Command string `json:"cmd"`
	Dir     string `json:"dir,omitempty"`
}

// CampaignManifest is uploaded to R2 by the coordinator and read by weft-agent run-campaign.
type CampaignManifest struct {
	Jobs            []AgentJob        `json:"jobs"`
	SelfDestructCmd string            `json:"self_destruct_cmd"`
	Env             map[string]string `json:"env,omitempty"`
}

// GenerateCampaignManifest produces JSON bytes for the campaign manifest
// that the agent fetches from R2 at startup.
func GenerateCampaignManifest(jobs []AgentJob, selfDestructCmd string, env map[string]string) ([]byte, error) {
	m := CampaignManifest{
		Jobs:            jobs,
		SelfDestructCmd: selfDestructCmd,
		Env:             env,
	}
	return json.Marshal(m)
}
