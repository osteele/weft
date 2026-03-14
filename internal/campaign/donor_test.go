package campaign

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestFindDonorOffer_MinimumInstances(t *testing.T) {
	// With fewer than 2 worker offers, donor should not be selected
	client := &cloud.MockClient{}
	offers := []cloud.Offer{{ProviderID: "1", DataCenter: "US-East"}}
	estimates := []CostEstimate{{DownloadBytes: 1e9, DownloadTime: 5 * time.Minute}}
	groups := []InstanceGroup{{GPUClass: "A100", Jobs: []*db.Job{{Inputs: []string{"hf:meta-llama/Llama-3-8B"}}}}}

	cfg, err := FindDonorOffer(client, offers, estimates, groups)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil donor config for single-offer campaign")
	}
}

func TestFindDonorOffer_SelectsCheapestCollocated(t *testing.T) {
	client := &cloud.MockClient{
		SearchOffersFunc: func(c cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{ProviderID: "cheap", GPUName: "RTX_3060", CostPerHour: 0.10, DataCenter: "US-East", Reliability: 0.99},
				{ProviderID: "expensive", GPUName: "RTX_4090", CostPerHour: 0.50, DataCenter: "US-East", Reliability: 0.99},
				{ProviderID: "other-dc", GPUName: "RTX_3060", CostPerHour: 0.05, DataCenter: "EU-West", Reliability: 0.99},
			}, nil
		},
	}

	offers := []cloud.Offer{
		{ProviderID: "w1", DataCenter: "US-East", CostPerHour: 1.50},
		{ProviderID: "w2", DataCenter: "US-East", CostPerHour: 1.50},
		{ProviderID: "w3", DataCenter: "EU-West", CostPerHour: 2.00},
	}
	// Workers need significant download time/bytes for donor to be cost-effective
	estimates := []CostEstimate{
		{DownloadBytes: 5e9, DownloadTime: 10 * time.Minute},
		{DownloadBytes: 5e9, DownloadTime: 10 * time.Minute},
		{DownloadBytes: 5e9, DownloadTime: 10 * time.Minute},
	}
	groups := []InstanceGroup{
		{GPUClass: "A100", Jobs: []*db.Job{{Inputs: []string{"hf:meta-llama/Llama-3-8B"}}}},
		{GPUClass: "A100", Jobs: []*db.Job{{Inputs: []string{"hf:meta-llama/Llama-3-8B"}}}},
		{GPUClass: "H100", Jobs: []*db.Job{{Inputs: []string{"hf:openai/whisper-large-v3"}}}},
	}

	cfg, err := FindDonorOffer(client, offers, estimates, groups)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil donor config")
	}
	if cfg.Offer.ProviderID != "cheap" {
		t.Errorf("expected cheapest collocated offer 'cheap', got %q", cfg.Offer.ProviderID)
	}
	if cfg.DataCenter != "US-East" {
		t.Errorf("expected US-East data center, got %q", cfg.DataCenter)
	}
}

func TestFindDonorOffer_NoCollocatedOffers(t *testing.T) {
	client := &cloud.MockClient{
		SearchOffersFunc: func(c cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{ProviderID: "other-dc", GPUName: "RTX_3060", CostPerHour: 0.10, DataCenter: "Asia"},
			}, nil
		},
	}

	offers := []cloud.Offer{
		{ProviderID: "w1", DataCenter: "US-East", CostPerHour: 1.50},
		{ProviderID: "w2", DataCenter: "US-East", CostPerHour: 1.50},
	}
	estimates := []CostEstimate{
		{DownloadBytes: 5e9, DownloadTime: 10 * time.Minute},
		{DownloadBytes: 5e9, DownloadTime: 10 * time.Minute},
	}
	groups := []InstanceGroup{{GPUClass: "A100"}, {GPUClass: "A100"}}

	cfg, err := FindDonorOffer(client, offers, estimates, groups)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil donor config when no collocated offers")
	}
}

func TestCollectHFModels(t *testing.T) {
	groups := []InstanceGroup{
		{Jobs: []*db.Job{
			{Inputs: []string{"hf:meta-llama/Llama-3-8B", "dataset:wikitext"}},
			{Inputs: []string{"hf:openai/whisper-large-v3"}},
		}},
		{Jobs: []*db.Job{
			{Inputs: []string{"hf:meta-llama/Llama-3-8B"}}, // duplicate
		}},
	}

	models := collectHFModels(groups)
	if len(models) != 2 {
		t.Fatalf("expected 2 unique HF models, got %d: %v", len(models), models)
	}

	found := map[string]bool{}
	for _, m := range models {
		found[m] = true
	}
	if !found["meta-llama/Llama-3-8B"] {
		t.Error("missing meta-llama/Llama-3-8B")
	}
	if !found["openai/whisper-large-v3"] {
		t.Error("missing openai/whisper-large-v3")
	}
}

func TestSeedWorkers_EmptyInputs(t *testing.T) {
	client := &cloud.MockClient{}

	// No workers
	err := SeedWorkers(client, nil, "donor1", nil, []string{"/root/.cache/uv"}, nil)
	if err != nil {
		t.Fatalf("expected no error with empty workers: %v", err)
	}

	// No cache paths
	err = SeedWorkers(client, nil, "donor1", []workerInfo{{ProviderID: "w1", DBID: 1}}, nil, nil)
	if err != nil {
		t.Fatalf("expected no error with empty cache paths: %v", err)
	}
}

func TestSeedWorkers_PhoneTreeFanOut(t *testing.T) {
	// Track which copies happen
	var copies []string
	client := &cloud.MockClient{
		CopyBetweenInstancesFunc: func(src, srcPath, dst, dstPath string) error {
			copies = append(copies, src+"->"+dst+":"+srcPath)
			return nil
		},
	}

	database := setupTestDB(t)

	// Create worker instances in DB
	workers := make([]workerInfo, 3)
	for i := range workers {
		id, err := db.CreateCloudInstance(database, &db.CloudInstance{
			Status:   "running",
			Provider: "mock",
		})
		if err != nil {
			t.Fatalf("create instance: %v", err)
		}
		_ = db.SetCloudInstanceProviderID(database, id, "w"+string(rune('1'+i)))
		workers[i] = workerInfo{ProviderID: "w" + string(rune('1'+i)), DBID: id}
	}

	err := SeedWorkers(client, database, "donor", workers, []string{"/root/.cache/uv"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All workers should have been copied to
	if len(copies) < 3 {
		t.Errorf("expected at least 3 copies, got %d: %v", len(copies), copies)
	}

	// Verify seed_copy_secs was set on all workers
	for _, w := range workers {
		inst, err := db.GetCloudInstance(database, w.DBID)
		if err != nil {
			t.Fatalf("get instance: %v", err)
		}
		if inst.SeedCopySecs == nil {
			t.Errorf("worker %d: seed_copy_secs not set", w.DBID)
		}
	}
}

func TestGenerateBootstrapScript_DonorMode(t *testing.T) {
	manifest := BootstrapManifest{
		AgentR2Key: "agent/v1/linux-amd64",
		Sources:    []SourceMapping{{R2Key: "sources/abc.tar.gz", RemoteDir: "/workspace/project"}},
		DonorMode:  true,
		HFModels:   []string{"meta-llama/Llama-3-8B", "openai/whisper-large-v3"},
		DonorID:    "42",
	}

	script := GenerateBootstrapScript(manifest)

	// Should contain uv sync
	if !strings.Contains(script, "uv sync") {
		t.Error("donor script should run uv sync")
	}

	// Should download HF models
	if !strings.Contains(script, "huggingface-cli download") {
		t.Error("donor script should download HF models")
	}
	if !strings.Contains(script, "meta-llama/Llama-3-8B") {
		t.Error("donor script should reference Llama model")
	}

	// Should write ready marker
	if !strings.Contains(script, "donor/42/.ready") {
		t.Error("donor script should write ready marker")
	}

	// Should NOT contain wrapper script
	if strings.Contains(script, "WRAPPER_EOF") {
		t.Error("donor script should not write a wrapper")
	}
	if strings.Contains(script, "nohup") {
		t.Error("donor script should not start a wrapper")
	}
}

func TestGenerateBootstrapScript_WorkerMode(t *testing.T) {
	manifest := BootstrapManifest{
		AgentR2Key:         "agent/v1/linux-amd64",
		Sources:            []SourceMapping{{R2Key: "sources/abc.tar.gz", RemoteDir: "/workspace/project"}},
		DBInstanceID:       7,
		MaxTimeSeconds:     3600,
		GracePeriodSeconds: 900,
		DonorMode:          false,
	}

	script := GenerateBootstrapScript(manifest)

	// Should launch weft-agent run-campaign via nohup
	if !strings.Contains(script, "nohup weft-agent run-campaign") {
		t.Error("worker script should launch weft-agent run-campaign")
	}
	if !strings.Contains(script, "--instance-id=7") {
		t.Error("worker script should pass instance ID")
	}
	if !strings.Contains(script, "--max-time=3600s") {
		t.Error("worker script should pass max-time")
	}
	if !strings.Contains(script, "--grace-period=900s") {
		t.Error("worker script should pass grace-period")
	}
	if strings.Contains(script, "--workspace=") {
		t.Error("worker script should not pass a workspace flag")
	}

	// Should NOT contain donor-specific items
	if strings.Contains(script, "uv sync") {
		t.Error("worker script should not run uv sync")
	}
	if strings.Contains(script, "donor/") {
		t.Error("worker script should not write donor ready marker")
	}
}

func TestGenerateBootstrapScript_WorkerWithHFModels(t *testing.T) {
	manifest := BootstrapManifest{
		AgentR2Key:   "agent/v1/linux-amd64",
		Sources:      []SourceMapping{{R2Key: "sources/abc.tar.gz", RemoteDir: "/workspace/project"}},
		DBInstanceID: 9,
		HFModels:     []string{"EleutherAI/pythia-1.4b", "gpt2-xl"},
		DonorMode:    false,
	}

	script := GenerateBootstrapScript(manifest)

	// Should download HF models before launching agent
	if !strings.Contains(script, "huggingface-cli download") {
		t.Error("worker script should download HF models")
	}
	if !strings.Contains(script, "EleutherAI/pythia-1.4b") {
		t.Error("worker script should reference Pythia model")
	}
	if !strings.Contains(script, "gpt2-xl") {
		t.Error("worker script should reference GPT-2 XL model")
	}

	// HF downloads should appear before the agent launch
	dlIdx := strings.Index(script, "huggingface-cli download")
	agentIdx := strings.Index(script, "nohup weft-agent run-campaign")
	if dlIdx >= agentIdx {
		t.Error("HF downloads should happen before agent launch")
	}
}
