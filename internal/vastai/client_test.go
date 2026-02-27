package vastai

import (
	"testing"
)

// Captured output from: vastai search offers --raw 'num_gpus=1 verified=true'
const searchOffersJSON = `[
  {
    "id": 12345678,
    "gpu_name": "RTX 4090",
    "num_gpus": 1,
    "gpu_ram": 24564,
    "dph_total": 0.45,
    "reliability2": 0.98,
    "inet_down": 500.0,
    "inet_up": 200.0,
    "disk_space": 100.0,
    "cuda_max_good": 12.2,
    "dlperf": 42.5,
    "verified": true
  },
  {
    "id": 87654321,
    "gpu_name": "A100 80GB",
    "num_gpus": 1,
    "gpu_ram": 81920,
    "dph_total": 1.20,
    "reliability2": 0.99,
    "inet_down": 1000.0,
    "inet_up": 500.0,
    "disk_space": 200.0,
    "cuda_max_good": 12.2,
    "dlperf": 85.0,
    "verified": true
  }
]`

// Captured output from: vastai show instances --raw
const showInstancesJSON = `[
  {
    "id": 99999,
    "actual_status": "running",
    "ssh_host": "ssh5.vast.ai",
    "ssh_port": 22222,
    "dph_total": 0.45
  }
]`

// Captured output from: vastai create instance 12345678 --image ... --raw
const createInstanceJSON = `{
  "new_contract": 99999,
  "success": true
}`

func TestParseSearchOffers(t *testing.T) {
	offers, err := parseOffers([]byte(searchOffersJSON))
	if err != nil {
		t.Fatalf("parseOffers: %v", err)
	}
	if len(offers) != 2 {
		t.Fatalf("expected 2 offers, got %d", len(offers))
	}

	rtx := offers[0]
	if rtx.ID != 12345678 {
		t.Errorf("offer[0].ID = %d, want 12345678", rtx.ID)
	}
	if rtx.GPUName != "RTX 4090" {
		t.Errorf("offer[0].GPUName = %q, want %q", rtx.GPUName, "RTX 4090")
	}
	if rtx.GPUMemMB != 24564 {
		t.Errorf("offer[0].GPUMemMB = %d, want 24564", rtx.GPUMemMB)
	}
	if rtx.GPUMemGB < 23.9 || rtx.GPUMemGB > 24.1 {
		t.Errorf("offer[0].GPUMemGB = %f, want ~24.0", rtx.GPUMemGB)
	}
	if rtx.CostPerHour != 0.45 {
		t.Errorf("offer[0].CostPerHour = %f, want 0.45", rtx.CostPerHour)
	}

	a100 := offers[1]
	if a100.GPUName != "A100 80GB" {
		t.Errorf("offer[1].GPUName = %q, want %q", a100.GPUName, "A100 80GB")
	}
	if a100.CostPerHour != 1.20 {
		t.Errorf("offer[1].CostPerHour = %f, want 1.20", a100.CostPerHour)
	}
}

func TestParseShowInstances(t *testing.T) {
	instances, err := parseInstances([]byte(showInstancesJSON))
	if err != nil {
		t.Fatalf("parseInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}

	inst := instances[0]
	if inst.ID != 99999 {
		t.Errorf("instance.ID = %d, want 99999", inst.ID)
	}
	if inst.Status != "running" {
		t.Errorf("instance.Status = %q, want %q", inst.Status, "running")
	}
	if inst.SSHHost != "ssh5.vast.ai" {
		t.Errorf("instance.SSHHost = %q, want %q", inst.SSHHost, "ssh5.vast.ai")
	}
	if inst.SSHPort != 22222 {
		t.Errorf("instance.SSHPort = %d, want 22222", inst.SSHPort)
	}
}

func TestParseCreateInstance(t *testing.T) {
	var resp struct {
		NewContract int  `json:"new_contract"`
		Success     bool `json:"success"`
	}
	if err := parseJSON([]byte(createInstanceJSON), &resp); err != nil {
		t.Fatalf("parse create response: %v", err)
	}
	if resp.NewContract != 99999 {
		t.Errorf("NewContract = %d, want 99999", resp.NewContract)
	}
	if !resp.Success {
		t.Error("Success = false, want true")
	}
}

func TestBuildSearchFilter(t *testing.T) {
	tests := []struct {
		name        string
		constraints OfferConstraints
		wantParts   []string
	}{
		{
			name:        "defaults only",
			constraints: OfferConstraints{},
			wantParts:   []string{"num_gpus=1", "direct_port_count>=1", "verified=true"},
		},
		{
			name: "with GPU class and memory",
			constraints: OfferConstraints{
				GPUClass:    "RTX_4090",
				MinGPUMemGB: 24,
			},
			wantParts: []string{"gpu_name=RTX_4090", "gpu_ram>=24576", "num_gpus=1"},
		},
		{
			name: "with reliability",
			constraints: OfferConstraints{
				MinReliability: 0.95,
			},
			wantParts: []string{"reliability2>=0.95", "num_gpus=1"},
		},
		{
			name: "multi-GPU",
			constraints: OfferConstraints{
				NumGPUs: 2,
			},
			wantParts: []string{"num_gpus=2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filter := buildSearchFilter(tt.constraints)
			for _, part := range tt.wantParts {
				if !containsSubstring(filter, part) {
					t.Errorf("filter %q missing part %q", filter, part)
				}
			}
		})
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
