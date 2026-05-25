package vastai

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestCreateInstanceEmptyOutputIsProviderRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	_, err := c.CreateInstance(12345, CreateOpts{Image: "ubuntu"})
	if !errors.Is(err, cloud.ErrProviderRejected) {
		t.Fatalf("CreateInstance error = %v, want provider rejected", err)
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("CreateInstance error = %v, want empty response detail", err)
	}
}

func TestCreateInstanceProviderErrorJSONIsProviderRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"error\": true, \"status_code\": 400, \"msg\": \"error 404/3603: no_such_ask\"}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	_, err := c.CreateInstance(12345, CreateOpts{Image: "ubuntu"})
	if !errors.Is(err, cloud.ErrProviderRejected) {
		t.Fatalf("CreateInstance error = %v, want provider rejected", err)
	}
	if !strings.Contains(err.Error(), "no_such_ask") {
		t.Fatalf("CreateInstance error = %v, want provider message", err)
	}
}

func TestCreateInstanceProviderRejectionLeadsWithReason(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"success\": false, \"msg\": \"error 404/3603: no_such_ask\"}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	_, err := c.CreateInstance(12345, CreateOpts{Image: "ubuntu"})
	if !errors.Is(err, cloud.ErrProviderRejected) {
		t.Fatalf("CreateInstance error = %v, want provider rejected", err)
	}
	// The actionable provider reason must lead so it survives one-line
	// truncation in compact views; the boilerplate trails it.
	msg := err.Error()
	reasonIdx := strings.Index(msg, "no_such_ask")
	boilerplateIdx := strings.Index(msg, "provider rejected instance creation")
	if reasonIdx < 0 || boilerplateIdx < 0 {
		t.Fatalf("CreateInstance error = %q, want both provider reason and boilerplate", msg)
	}
	if reasonIdx > boilerplateIdx {
		t.Fatalf("CreateInstance error = %q, want provider reason before boilerplate", msg)
	}
}

func TestCreateInstanceCLITimeoutIsProviderCommandTimeout(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nsleep 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub, CLITimeout: 10 * time.Millisecond}
	_, err := c.CreateInstance(12345, CreateOpts{Image: "ubuntu"})
	if !errors.Is(err, cloud.ErrProviderCommandTimeout) {
		t.Fatalf("CreateInstance error = %v, want provider command timeout", err)
	}
	if errors.Is(err, cloud.ErrProviderRejected) {
		t.Fatalf("CreateInstance error = %v, must not be provider rejected", err)
	}
	if !strings.Contains(err.Error(), "vastai create instance 12345 timed out") {
		t.Fatalf("CreateInstance error = %v, want timeout detail", err)
	}
}

// TestRunWithTimeoutHonorsPerCallTimeout verifies that runWithTimeout's
// commandTimeout parameter actually governs the deadline when the test-only
// CLITimeout override is not set. This is the mechanism CreateInstance and
// AttachSSH rely on for their 2-minute per-call timeouts.
func TestRunWithTimeoutHonorsPerCallTimeout(t *testing.T) {
	dir := t.TempDir()
	stubSleep := filepath.Join(dir, "vastai_sleep")
	if err := os.WriteFile(stubSleep, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	stubFast := filepath.Join(dir, "vastai_fast")
	if err := os.WriteFile(stubFast, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	// Per-call timeout shorter than the stub's sleep should fire as a provider
	// command timeout, even though the default cliTimeout (30s) would not.
	c := &Client{CLIPath: stubSleep}
	if _, err := c.runWithTimeout(50*time.Millisecond, "create", "instance", "12345"); !errors.Is(err, cloud.ErrProviderCommandTimeout) {
		t.Fatalf("runWithTimeout(short) err = %v, want ErrProviderCommandTimeout", err)
	}

	// A fast-exit stub completes well within the per-call timeout. Confirms
	// runWithTimeout does not impose extra delay beyond the deadline. Generous
	// budget keeps the test stable under CI load and -race overhead.
	c = &Client{CLIPath: stubFast}
	if _, err := c.runWithTimeout(5*time.Second, "anything"); err != nil {
		t.Fatalf("runWithTimeout(fast) err = %v, want nil", err)
	}

	// CLITimeout always wins over commandTimeout — required for tests like
	// TestCreateInstanceCLITimeoutIsProviderCommandTimeout to keep working
	// now that CreateInstance opts into a 2-minute commandTimeout.
	c = &Client{CLIPath: stubSleep, CLITimeout: 10 * time.Millisecond}
	if _, err := c.runWithTimeout(10*time.Second, "create", "instance", "12345"); !errors.Is(err, cloud.ErrProviderCommandTimeout) {
		t.Fatalf("runWithTimeout with CLITimeout override err = %v, want ErrProviderCommandTimeout (CLITimeout must take precedence)", err)
	}
}

// TestCreateInstanceAndAttachSSHUseLongerTimeoutConstants asserts that the
// long-timeout opt-in is real: the createInstanceTimeout and attachSSHTimeout
// constants must be strictly greater than the default cliTimeout so a slow
// vast.ai API hour doesn't convert into a hard provider_timeout.
func TestCreateInstanceAndAttachSSHUseLongerTimeoutConstants(t *testing.T) {
	if createInstanceTimeout <= cliTimeout {
		t.Errorf("createInstanceTimeout = %s, want > cliTimeout (%s)", createInstanceTimeout, cliTimeout)
	}
	if attachSSHTimeout <= cliTimeout {
		t.Errorf("attachSSHTimeout = %s, want > cliTimeout (%s)", attachSSHTimeout, cliTimeout)
	}
}

func TestAvailableRejectsProviderErrorJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"error\": true, \"status_code\": 400, \"msg\": \"owner: Extra inputs are not permitted\"}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	err := c.Available()
	if err == nil {
		t.Fatal("Available() error = nil, want provider error")
	}
	if !strings.Contains(err.Error(), "owner: Extra inputs are not permitted") {
		t.Fatalf("Available() error = %v, want provider message", err)
	}
}

func TestShowUserProviderErrorJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"error\": true, \"status_code\": 400, \"msg\": \"owner: Extra inputs are not permitted\"}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	_, err := c.ShowUser()
	if err == nil {
		t.Fatal("ShowUser() error = nil, want provider error")
	}
	if !strings.Contains(err.Error(), "owner: Extra inputs are not permitted") {
		t.Fatalf("ShowUser() error = %v, want provider message", err)
	}
	if strings.Contains(err.Error(), "parse user") {
		t.Fatalf("ShowUser() error = %v, should not report parse failure for provider error", err)
	}
}

func TestShowUserEmptyOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	_, err := c.ShowUser()
	if err == nil {
		t.Fatal("ShowUser() error = nil, want empty response error")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("ShowUser() error = %v, want empty response detail", err)
	}
	if strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Fatalf("ShowUser() error = %v, should not expose JSON EOF", err)
	}
}

func TestSearchOffersEmptyOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	_, err := c.SearchOffers(OfferConstraints{})
	if err == nil {
		t.Fatal("SearchOffers() error = nil, want empty response error")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("SearchOffers() error = %v, want empty response detail", err)
	}
	if strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Fatalf("SearchOffers() error = %v, should not expose JSON EOF", err)
	}
}

func TestSearchOffersPostFiltersMinCUDA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := `#!/bin/sh
cat <<'JSON'
[
  {"id": 1, "gpu_name": "Tesla V100", "num_gpus": 1, "gpu_ram": 32768, "cuda_max_good": 12.2, "dph_total": 0.14, "reliability2": 0.99},
  {"id": 2, "gpu_name": "Tesla V100", "num_gpus": 1, "gpu_ram": 32768, "cuda_max_good": 12.8, "dph_total": 0.19, "reliability2": 0.99}
]
JSON
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	offers, err := c.SearchOffers(OfferConstraints{MinCUDAVersion: "12.8"})
	if err != nil {
		t.Fatalf("SearchOffers: %v", err)
	}
	if len(offers) != 1 || offers[0].ID != 2 {
		t.Fatalf("offers = %#v, want only CUDA 12.8 offer", offers)
	}
}

func TestShowUserFromAPI(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credit": 12.34, "balance": 12.34, "email": "user@example.com", "can_pay": true}`))
	}))
	defer server.Close()

	user, err := showUserFromAPI("test-key", server.URL)
	if err != nil {
		t.Fatalf("showUserFromAPI: %v", err)
	}
	if user.Credit != 12.34 {
		t.Fatalf("Credit = %v, want 12.34", user.Credit)
	}
}

func TestShowUserFromAPIProviderError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": true, "status_code": 400, "msg": "owner: Extra inputs are not permitted"}`))
	}))
	defer server.Close()

	_, err := showUserFromAPI("test-key", server.URL)
	if err == nil {
		t.Fatal("showUserFromAPI error = nil, want provider error")
	}
	if !strings.Contains(err.Error(), "owner: Extra inputs are not permitted") {
		t.Fatalf("showUserFromAPI error = %v, want provider message", err)
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
			wantParts: []string{`gpu_name="RTX 4090"`, "gpu_ram>=24", "num_gpus=1"},
		},
		{
			name: "v100 maps to current Vast offer name",
			constraints: OfferConstraints{
				GPUClass:    "v100",
				MinGPUMemGB: 22,
			},
			wantParts: []string{`gpu_name="Tesla V100"`, "gpu_ram>=22", "num_gpus=1"},
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

func TestBuildSearchFilter_MinDriverVersion(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{MinDriverVersion: 535})
	if !strings.Contains(filter, "driver_version>=535") {
		t.Errorf("filter %q missing driver_version>=535", filter)
	}
}

func TestBuildSearchFilter_MinCUDAVersion(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{MinCUDAVersion: "12.8"})
	if !strings.Contains(filter, "cuda_vers>=12.8") {
		t.Errorf("filter %q missing cuda_vers>=12.8", filter)
	}
}

func TestBuildSearchFilter_NoCPUCores(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{})
	if strings.Contains(filter, "cpu_cores_effective") {
		t.Errorf("filter %q should not contain cpu_cores_effective when MinCPUCoresEffective=0", filter)
	}
}

func TestBuildSearchFilter_IgnoresMaxGPUMem(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{MinGPUMemGB: 24, MaxGPUMemGB: 48})
	if !strings.Contains(filter, "gpu_ram>=24") {
		t.Errorf("filter %q should contain gpu_ram>=24", filter)
	}
	if strings.Contains(filter, "gpu_ram<=") {
		t.Errorf("filter %q should not contain gpu_ram<=", filter)
	}
}

func TestBuildSearchFilter_NoMaxGPUMem(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{MinGPUMemGB: 24})
	if strings.Contains(filter, "gpu_ram<=") {
		t.Errorf("filter %q should not contain gpu_ram<= when MaxGPUMemGB=0", filter)
	}
}

// Regression: gpu_frac=1 must NOT be in the filter. It excluded all multi-GPU
// host families (A100/H100 SXM) because their per-GPU offers report
// gpu_frac < 1 (e.g. 0.125 on an 8x A100 host), making num_gpus=1 jobs
// against a100/h100 unsatisfiable. See diagnosis 2026-04-23.
func TestBuildSearchFilter_NoGPUFrac(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{GPUClass: "A100", MinGPUMemGB: 42, MinReliability: 0.95})
	if strings.Contains(filter, "gpu_frac") {
		t.Errorf("filter %q must not include gpu_frac (excludes multi-GPU-host A100/H100 supply)", filter)
	}
}

func TestBuildCreateArgs_InterruptibleBid(t *testing.T) {
	args := buildCreateArgs(12345, CreateOpts{
		InstanceType: cloud.InstanceTypeInterruptible,
		MaxBidPrice:  0.42,
		Image:        "nvidia/cuda:12.4.1-runtime-ubuntu22.04",
	})
	joined := strings.Join(args, " ")
	// `create instance` has no --type flag; the presence of --bid_price is
	// what makes the instance interruptible.
	for _, part := range []string{"create instance 12345", "--bid_price 0.4200", "--image nvidia/cuda:12.4.1-runtime-ubuntu22.04"} {
		if !strings.Contains(joined, part) {
			t.Fatalf("create args %q missing %q", joined, part)
		}
	}
	for _, forbidden := range []string{"--type bid", "--price 0.4200"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("create args %q must not include %q (not valid for `vastai create instance`)", joined, forbidden)
		}
	}
}

func TestBuildCreateArgs_OnDemandOmitsBid(t *testing.T) {
	args := buildCreateArgs(12345, CreateOpts{
		InstanceType: cloud.InstanceTypeOnDemand,
		Image:        "nvidia/cuda:12.4.1-runtime-ubuntu22.04",
	})
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--bid_price") {
		t.Errorf("on-demand create args %q must not include --bid_price", joined)
	}
}

func TestBuildCreateArgs_RegistryLogin(t *testing.T) {
	args := buildCreateArgs(12345, CreateOpts{
		Image: "ghcr.io/org/image:tag",
		RegistryAuth: &cloud.RegistryAuth{
			Host:     "ghcr.io",
			Username: "osteele",
			Password: "secret",
		},
	})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--login -u osteele -p secret ghcr.io") {
		t.Fatalf("create args %q missing registry login", joined)
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

func TestIsProviderRejectedCreateError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{err: nil, want: false},
		{err: errors.New("create instance 27681642: exit -1 (no stderr)"), want: false},
		{err: errors.New("search offers --raw: exit -1 (no stderr)"), want: false},
		{err: errors.New("create instance 123: insufficient balance"), want: false},
		{err: errors.New("provider command timed out: vastai create instance 123 timed out after 30s"), want: false},
	}

	for _, tt := range tests {
		if got := isProviderRejectedCreateError(tt.err); got != tt.want {
			t.Fatalf("isProviderRejectedCreateError(%v) = %v, want %v", tt.err, got, tt.want)
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

// Without -y, vastai destroy reads stdin for confirmation, gets EOF, prints
// "Aborted." and exits 0 — silently no-opping every destroy call.
func TestDestroyInstancePassesYesFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	c := &Client{CLIPath: stub}
	if err := c.DestroyInstance(12345); err != nil {
		t.Fatalf("DestroyInstance: %v", err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	if !strings.Contains(string(got), "\n-y\n") && !strings.Contains(string(got), "\n--yes\n") {
		t.Fatalf("DestroyInstance must pass -y/--yes; got args:\n%s", got)
	}
}
