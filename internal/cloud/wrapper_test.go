package cloud

import (
	"encoding/json"
	"testing"
)

func TestGenerateCampaignManifest_Basic(t *testing.T) {
	jobs := []AgentJob{
		{ID: 42, Command: "python train.py"},
		{ID: 43, Command: "python eval.py", Dir: "/custom/dir"},
	}
	data, err := GenerateCampaignManifest(jobs, `vastai destroy instance "123"`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m CampaignManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if len(m.Jobs) != 2 {
		t.Errorf("expected 2 jobs, got %d", len(m.Jobs))
	}
	if m.Jobs[0].ID != 42 {
		t.Errorf("expected job ID 42, got %d", m.Jobs[0].ID)
	}
	if m.Jobs[1].Dir != "/custom/dir" {
		t.Errorf("expected job dir /custom/dir, got %s", m.Jobs[1].Dir)
	}
	if m.SelfDestructCmd != `vastai destroy instance "123"` {
		t.Errorf("unexpected self-destruct cmd: %s", m.SelfDestructCmd)
	}
}

func TestGenerateCampaignManifest_WithEnv(t *testing.T) {
	jobs := []AgentJob{{ID: 1, Command: "echo hi"}}
	env := map[string]string{
		"HF_TOKEN": "hf_test123",
	}
	data, err := GenerateCampaignManifest(jobs, "true", env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m CampaignManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if m.Env["HF_TOKEN"] != "hf_test123" {
		t.Errorf("expected HF_TOKEN=hf_test123, got %s", m.Env["HF_TOKEN"])
	}
}

func TestGenerateCampaignManifest_NilEnv(t *testing.T) {
	data, err := GenerateCampaignManifest([]AgentJob{{ID: 1, Command: "echo hi"}}, "true", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m CampaignManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if m.Env != nil {
		t.Errorf("expected nil env, got %v", m.Env)
	}
}
