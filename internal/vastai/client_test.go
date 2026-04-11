package vastai

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
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
    "dph_total": 0.45,
    "disk_space": 150.0,
    "cpu_cores_effective": 24.0,
    "cpu_name": "AMD EPYC 7763",
    "cpu_ram": 131072.0
  }
]`

// Captured output from: vastai create instance 12345678 --image ... --raw
const createInstanceJSON = `{
  "new_contract": 99999,
  "success": true
}`

func TestParseSearchOffers(t *testing.T) {
	var offers []Offer
	if err := json.Unmarshal([]byte(searchOffersJSON), &offers); err != nil {
		t.Fatalf("unmarshal offers: %v", err)
	}
	for i := range offers {
		offers[i].GPUMemGB = float64(offers[i].GPUMemMB) / 1024.0
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
	var instances []Instance
	if err := json.Unmarshal([]byte(showInstancesJSON), &instances); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 instance, got %d", len(instances))
	}

	inst := instances[0]
	if inst.ID != 99999 {
		t.Errorf("instance.ID = %d, want 99999", inst.ID)
	}
	if inst.Status != cloud.ProviderStatusRunning {
		t.Errorf("instance.Status = %q, want %q", inst.Status, cloud.ProviderStatusRunning)
	}
	if inst.SSHHost != "ssh5.vast.ai" {
		t.Errorf("instance.SSHHost = %q, want %q", inst.SSHHost, "ssh5.vast.ai")
	}
	if inst.SSHPort != 22222 {
		t.Errorf("instance.SSHPort = %d, want 22222", inst.SSHPort)
	}
	if inst.DiskSpace != 150.0 {
		t.Errorf("instance.DiskSpace = %f, want 150.0", inst.DiskSpace)
	}
	if inst.CPUCores != 24.0 {
		t.Errorf("instance.CPUCores = %f, want 24.0", inst.CPUCores)
	}
	if inst.CPUName != "AMD EPYC 7763" {
		t.Errorf("instance.CPUName = %q, want %q", inst.CPUName, "AMD EPYC 7763")
	}
	if inst.CPURAMMB != 131072.0 {
		t.Errorf("instance.CPURAMMB = %f, want 131072.0", inst.CPURAMMB)
	}
}

func TestParseCreateInstance(t *testing.T) {
	var resp struct {
		NewContract int  `json:"new_contract"`
		Success     bool `json:"success"`
	}
	if err := json.Unmarshal([]byte(createInstanceJSON), &resp); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
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
			wantParts:   []string{"num_gpus=1", "direct_port_count>=1", "verified=true", "gpu_frac=1"},
		},
		{
			name: "with GPU class and memory",
			constraints: OfferConstraints{
				GPUClass:    "RTX_4090",
				MinGPUMemGB: 24,
			},
			wantParts: []string{`gpu_name="RTX 4090"`, "gpu_ram>=24", "num_gpus=1"},
		},
		{
			name: "with reliability",
			constraints: OfferConstraints{
				MinReliability: 0.95,
			},
			wantParts: []string{"reliability>=0.95", "num_gpus=1"},
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
			filter, _ := buildSearchFilter(tt.constraints)
			for _, part := range tt.wantParts {
				if !strings.Contains(filter, part) {
					t.Errorf("filter %q missing part %q", filter, part)
				}
			}
		})
	}
}

func TestBuildSearchFilter_CPUCores(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{
		MinCPUCoresEffective: 16,
	})
	if !strings.Contains(filter, "cpu_cores_effective>=16") {
		t.Errorf("filter %q missing cpu_cores_effective>=16", filter)
	}
}

func TestBuildSearchFilter_NoCPUCores(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{})
	if strings.Contains(filter, "cpu_cores_effective") {
		t.Errorf("filter %q should not contain cpu_cores_effective when MinCPUCoresEffective=0", filter)
	}
}

func TestBuildSearchFilter_MaxGPUMem(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{MinGPUMemGB: 24, MaxGPUMemGB: 48})
	if !strings.Contains(filter, "gpu_ram>=24") {
		t.Errorf("filter %q should contain gpu_ram>=24", filter)
	}
	if !strings.Contains(filter, "gpu_ram<=48") {
		t.Errorf("filter %q should contain gpu_ram<=48", filter)
	}
}

func TestBuildSearchFilter_NoMaxGPUMem(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{MinGPUMemGB: 24})
	if strings.Contains(filter, "gpu_ram<=") {
		t.Errorf("filter %q should not contain gpu_ram<= when MaxGPUMemGB=0", filter)
	}
}

func TestIsUnavailableOfferError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{err: nil, want: false},
		{err: errors.New("create instance 123: ask 123 no longer exists"), want: true},
		{err: errors.New("create instance 123: offer 123 not found"), want: true},
		{err: errors.New("create instance 123: machine is no longer available"), want: true},
		{err: errors.New("create instance 123: insufficient balance"), want: false},
	}

	for _, tt := range tests {
		if got := isUnavailableOfferError(tt.err); got != tt.want {
			t.Fatalf("isUnavailableOfferError(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestExtractCLIError(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{"empty", "", ""},
		{"json object", `{"success": true}`, ""},
		{"json array", `[{"id": 1}]`, ""},
		{"billing error", "failed with error 400: Your account lacks credit; see the billing page.\n", "failed with error 400: Your account lacks credit; see the billing page."},
		{"plain error", "some unexpected error message", "some unexpected error message"},
		{"whitespace trimmed", "  error with spaces  \n", "error with spaces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCLIError([]byte(tt.out))
			if got != tt.want {
				t.Errorf("extractCLIError(%q) = %q, want %q", tt.out, got, tt.want)
			}
		})
	}
}

func TestIsVastAuthError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{err: errors.New("show user --raw: unauthorized"), want: true},
		{err: errors.New("invalid API key"), want: true},
		{err: errors.New("connection refused"), want: false},
	}
	for _, tt := range tests {
		if got := isVastAuthError(tt.err); got != tt.want {
			t.Fatalf("isVastAuthError(%q)=%v want %v", tt.err, got, tt.want)
		}
	}
}

func TestIsVastTransientAvailabilityError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{err: errors.New("failed to resolve host"), want: true},
		{err: errors.New("context deadline exceeded"), want: true},
		{err: errors.New("network is unreachable"), want: true},
		{err: errors.New("invalid API key"), want: false},
	}
	for _, tt := range tests {
		if got := isVastTransientAvailabilityError(tt.err); got != tt.want {
			t.Fatalf("isVastTransientAvailabilityError(%q)=%v want %v", tt.err, got, tt.want)
		}
	}
}

func TestRecentlyAvailable(t *testing.T) {
	availabilityState.mu.Lock()
	availabilityState.lastSuccess = time.Time{}
	availabilityState.mu.Unlock()

	if recentlyAvailable(2 * time.Minute) {
		t.Fatalf("recentlyAvailable should be false with zero lastSuccess")
	}
	markAvailabilitySuccess(time.Now().Add(-30 * time.Second))
	if !recentlyAvailable(2 * time.Minute) {
		t.Fatalf("recentlyAvailable should be true within grace window")
	}
	if recentlyAvailable(10 * time.Second) {
		t.Fatalf("recentlyAvailable should be false outside grace window")
	}
}
