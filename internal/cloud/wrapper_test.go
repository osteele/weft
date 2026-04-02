package cloud

import (
	"encoding/json"
	"testing"
)

func TestCampaignManifest_RoundTrip(t *testing.T) {
	m := CampaignManifest{
		Jobs: []AgentJob{
			{
				ID:         42,
				Command:    "python train.py",
				OutputDirs: []string{"results/"},
				Produces:   []string{"results/model.pt"},
				Needs:      []string{"inputs/data.csv:41"},
			},
			{ID: 43, Command: "python eval.py", Dir: "/custom/dir"},
		},
		SelfDestructCmd: `vastai destroy instance "123"`,
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got CampaignManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.Jobs) != 2 {
		t.Errorf("expected 2 jobs, got %d", len(got.Jobs))
	}
	if got.Jobs[0].ID != 42 {
		t.Errorf("expected job ID 42, got %d", got.Jobs[0].ID)
	}
	if got.Jobs[1].Dir != "/custom/dir" {
		t.Errorf("expected job dir /custom/dir, got %s", got.Jobs[1].Dir)
	}
	if len(got.Jobs[0].OutputDirs) != 1 || got.Jobs[0].OutputDirs[0] != "results/" {
		t.Errorf("unexpected output dirs: %v", got.Jobs[0].OutputDirs)
	}
	if len(got.Jobs[0].Produces) != 1 || got.Jobs[0].Produces[0] != "results/model.pt" {
		t.Errorf("unexpected produces: %v", got.Jobs[0].Produces)
	}
	if len(got.Jobs[0].Needs) != 1 || got.Jobs[0].Needs[0] != "inputs/data.csv:41" {
		t.Errorf("unexpected needs: %v", got.Jobs[0].Needs)
	}
	if got.SelfDestructCmd != `vastai destroy instance "123"` {
		t.Errorf("unexpected self-destruct cmd: %s", got.SelfDestructCmd)
	}
}

func TestCampaignManifest_Env(t *testing.T) {
	m := CampaignManifest{
		Jobs: []AgentJob{{ID: 1, Command: "echo hi"}},
		Env:  map[string]string{"HF_TOKEN": "hf_test123"},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got CampaignManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Env["HF_TOKEN"] != "hf_test123" {
		t.Errorf("expected HF_TOKEN=hf_test123, got %s", got.Env["HF_TOKEN"])
	}
}

func TestCampaignManifest_NilEnvOmitted(t *testing.T) {
	m := CampaignManifest{
		Jobs: []AgentJob{{ID: 1, Command: "echo hi"}},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got CampaignManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Env != nil {
		t.Errorf("expected nil env, got %v", got.Env)
	}
}
