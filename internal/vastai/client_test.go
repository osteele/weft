package vastai

import (
	"encoding/json"
	"errors"
	"fmt"
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
    "bw_nvlink": 478.116,
    "disk_space": 100.0,
    "cuda_max_good": 12.2,
    "datacenter": true,
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
    "bw_nvlink": 0.0,
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
	if !rtx.DatacenterDriver {
		t.Errorf("offer[0].DatacenterDriver = false, want true")
	}
	if rtx.NVLinkBandwidth == nil || *rtx.NVLinkBandwidth != 478.116 {
		t.Errorf("offer[0].NVLinkBandwidth = %v, want 478.116", rtx.NVLinkBandwidth)
	}

	a100 := offers[1]
	if a100.GPUName != "A100 80GB" {
		t.Errorf("offer[1].GPUName = %q, want %q", a100.GPUName, "A100 80GB")
	}
	if a100.CostPerHour != 1.20 {
		t.Errorf("offer[1].CostPerHour = %f, want 1.20", a100.CostPerHour)
	}
	if a100.NVLinkBandwidth == nil || *a100.NVLinkBandwidth != 0 {
		t.Errorf("offer[1].NVLinkBandwidth = %v, want reported zero", a100.NVLinkBandwidth)
	}
}

func TestSearchOffersPricesRequestedStorage(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	argsPath := filepath.Join(dir, "args")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$VAST_ARGS_FILE"
printf '[]\n'
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("VAST_ARGS_FILE", argsPath)

	c := &Client{CLIPath: stub}
	if _, err := c.SearchOffers(OfferConstraints{MinDiskGB: 1200}); err != nil {
		t.Fatalf("SearchOffers: %v", err)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--storage 1200") {
		t.Fatalf("SearchOffers args = %q, want requested storage pricing", joined)
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

func TestWaitReadyReturnsNotFoundAfterInstanceDisappears(t *testing.T) {
	t.Parallel()
	calls := 0
	show := func(id int) (*Instance, error) {
		if id != 99999 {
			t.Fatalf("instance ID = %d, want 99999", id)
		}
		calls++
		if calls == 1 {
			return &Instance{ID: 99999, Status: cloud.ProviderStatusCreating}, nil
		}
		return nil, fmt.Errorf("instance 99999: %w", cloud.ErrInstanceNotFound)
	}

	_, err := waitReady(99999, 100*time.Millisecond, time.Millisecond, show)
	if !errors.Is(err, cloud.ErrInstanceNotFound) {
		t.Fatalf("WaitReady error = %v, want ErrInstanceNotFound", err)
	}
}

func TestWaitReadyAllowsInitialNotFound(t *testing.T) {
	t.Parallel()
	calls := 0
	show := func(id int) (*Instance, error) {
		if id != 99999 {
			t.Fatalf("instance ID = %d, want 99999", id)
		}
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("instance 99999: %w", cloud.ErrInstanceNotFound)
		}
		return &Instance{ID: 99999, Status: cloud.ProviderStatusRunning}, nil
	}

	inst, err := waitReady(99999, 100*time.Millisecond, time.Millisecond, show)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if inst.ID != 99999 || inst.Status != cloud.ProviderStatusRunning {
		t.Fatalf("instance = %+v, want running 99999", inst)
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
	// Cannot run in parallel: shares package-level probeCreditBalance with
	// TestCreateInstanceEmptyResponse* below.
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

func TestCreateInstanceAccountCreditExhaustedFromCLIMessage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	// vastai surfaces credit-exhaustion as a plain-text CLI error on stdout
	// even though the CLI exits non-zero. Simulate the exit-1 + stderr path.
	script := "#!/bin/sh\necho 'failed with error 400: Your account lacks credit; see the billing page.' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	_, err := c.CreateInstance(12345, CreateOpts{Image: "ubuntu"})
	if !errors.Is(err, cloud.ErrAccountCreditExhausted) {
		t.Fatalf("CreateInstance error = %v, want account credit exhausted", err)
	}
	if errors.Is(err, cloud.ErrProviderRejected) {
		t.Fatalf("CreateInstance error = %v, must not also be provider rejected", err)
	}
}

func TestCreateInstanceEmptyResponseWithExhaustedCreditIsAccountCreditExhausted(t *testing.T) {
	// Cannot run in parallel: modifies package-level probeCreditBalance.
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	prev := probeCreditBalance
	probeCreditBalance = func(_ *Client) (float64, error) { return 0, nil }
	t.Cleanup(func() { probeCreditBalance = prev })

	c := &Client{CLIPath: stub}
	_, err := c.CreateInstance(12345, CreateOpts{Image: "ubuntu"})
	if !errors.Is(err, cloud.ErrAccountCreditExhausted) {
		t.Fatalf("CreateInstance error = %v, want account credit exhausted", err)
	}
	if !strings.Contains(err.Error(), "$0.00") {
		t.Fatalf("CreateInstance error = %v, want balance in detail", err)
	}
}

func TestCreateInstanceEmptyResponseWithProbeFailureFallsBackToProviderRejected(t *testing.T) {
	// Cannot run in parallel: modifies package-level probeCreditBalance.
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	prev := probeCreditBalance
	probeCreditBalance = func(_ *Client) (float64, error) {
		return 0, errors.New("show user: network unreachable")
	}
	t.Cleanup(func() { probeCreditBalance = prev })

	c := &Client{CLIPath: stub}
	_, err := c.CreateInstance(12345, CreateOpts{Image: "ubuntu"})
	if !errors.Is(err, cloud.ErrProviderRejected) {
		t.Fatalf("CreateInstance error = %v, want provider rejected (probe failed, unknown credit)", err)
	}
	if errors.Is(err, cloud.ErrAccountCreditExhausted) {
		t.Fatalf("CreateInstance error = %v, must not attribute to credit when probe failed", err)
	}
}

func TestCreateInstanceSuccessFalseWithCreditMessageIsAccountCreditExhausted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"success\": false, \"msg\": \"Your account lacks credit\"}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	_, err := c.CreateInstance(12345, CreateOpts{Image: "ubuntu"})
	if !errors.Is(err, cloud.ErrAccountCreditExhausted) {
		t.Fatalf("CreateInstance error = %v, want account credit exhausted", err)
	}
}

func TestIsAccountCreditError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg  string
		want bool
	}{
		{"", false},
		{"some random error", false},
		{"failed with error 400: Your account lacks credit", true},
		{"YOUR ACCOUNT LACKS CREDIT; SEE THE BILLING PAGE", true},
		{"insufficient balance for offer 12345", true},
		{"Insufficient credit", true},
		{"account credit was exhausted", true},
		{"no_such_ask", false},
		{"machine busy", false},
	}
	for _, tc := range cases {
		if got := isAccountCreditError(tc.msg); got != tc.want {
			t.Errorf("isAccountCreditError(%q) = %v, want %v", tc.msg, got, tc.want)
		}
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
	boilerplateIdx := strings.Index(msg, "provider rejected request")
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

// TestInstancePollAndOfferSearchTimeoutsExceedDefault pins the relationship
// between the per-call timeouts for status polling / offer search and the
// default cliTimeout. `vastai show instances` and `vastai search offers`
// routinely exceed 30s on slow networks; regressing them back to the default
// re-creates the "poll timeout conflated with instance dead" failure mode.
func TestInstancePollAndOfferSearchTimeoutsExceedDefault(t *testing.T) {
	t.Parallel()
	if instancePollTimeout <= cliTimeout {
		t.Errorf("instancePollTimeout = %s, want > default cliTimeout %s", instancePollTimeout, cliTimeout)
	}
	if offerSearchTimeout <= cliTimeout {
		t.Errorf("offerSearchTimeout = %s, want > default cliTimeout %s", offerSearchTimeout, cliTimeout)
	}
}

// TestInstancePollHonorsCLITimeoutOverride verifies the test/config override
// precedence for the polling calls that now opt into instancePollTimeout /
// offerSearchTimeout: a non-zero Client.CLITimeout must still win, and the
// timeout must surface as ErrProviderCommandTimeout.
func TestInstancePollHonorsCLITimeoutOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	c := &Client{CLIPath: stub, CLITimeout: 10 * time.Millisecond}

	if _, err := c.ShowInstance(12345); !errors.Is(err, cloud.ErrProviderCommandTimeout) {
		t.Errorf("ShowInstance err = %v, want ErrProviderCommandTimeout (CLITimeout must take precedence)", err)
	}
	if _, err := c.ListAllInstances(); !errors.Is(err, cloud.ErrProviderCommandTimeout) {
		t.Errorf("ListAllInstances err = %v, want ErrProviderCommandTimeout (CLITimeout must take precedence)", err)
	}
	if _, err := c.SearchOffers(OfferConstraints{}); !errors.Is(err, cloud.ErrProviderCommandTimeout) {
		t.Errorf("SearchOffers err = %v, want ErrProviderCommandTimeout (CLITimeout must take precedence)", err)
	}
}

// TestSearchOffersSurfacesStderrErrorOnZeroExit covers the case where the
// vastai CLI exits 0 with empty stdout but writes an API error payload to
// stderr — observed with vastai 1.0.7's "driver_vers gte None" regression and
// with rejections like "bogus_field is not a valid search key". Before the
// fix this surfaced as a generic "provider returned empty response" with no
// detail; now it propagates as ErrProviderRejected carrying the actual msg.
func TestSearchOffersSurfacesStderrErrorOnZeroExit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	// Mimic vastai's observed behavior: a "Warning:" preamble line followed by
	// the JSON error payload, all on stderr, with empty stdout and exit 0.
	script := `#!/bin/sh
printf '%s\n' 'Warning: Unrecognized field: bogus_field, see list of recognized fields.' >&2
printf '%s\n' '{"error": true, "status_code": 400, "msg": "bogus_field is not a valid search key"}' >&2
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c := &Client{CLIPath: stub}
	_, err := c.SearchOffers(OfferConstraints{GPUClass: "nvidia", MinGPUMemGB: 8, NumGPUs: 1})
	if err == nil {
		t.Fatal("SearchOffers err = nil, want error")
	}
	if !errors.Is(err, cloud.ErrProviderRejected) {
		t.Fatalf("SearchOffers err = %v, want errors.Is(ErrProviderRejected)=true", err)
	}
	if !strings.Contains(err.Error(), "bogus_field is not a valid search key") {
		t.Fatalf("SearchOffers err = %q, want CLI msg in detail", err)
	}
	// Regression: the unhelpful "provider returned empty response" branch must
	// not eat this case anymore.
	if strings.Contains(err.Error(), "provider returned empty response") {
		t.Fatalf("SearchOffers err = %q, must not collapse to generic empty-response message", err)
	}
	// Regression for the duplicate-prefix UX bug: the user-visible error must
	// not contain "search offers: search offers" or expose the internal
	// "--raw" flag. The operation label is added once by runWithTimeout.
	msg := err.Error()
	if strings.Contains(msg, "search offers: search offers") {
		t.Fatalf("SearchOffers err = %q, must not double-prefix the operation label", msg)
	}
	if strings.Contains(msg, "--raw") {
		t.Fatalf("SearchOffers err = %q, must not leak the internal --raw flag into user-facing text", msg)
	}
}

// TestSearchOffersCarriesProviderErrorWithFingerprint is the integration-level
// regression for error-class coalescing: when Vast.ai 400s with the
// driver_vers shape, SearchOffers must surface a *cloud.ProviderError whose
// Fingerprint is the stable coalescing key. The TUI/CLI layers downstream
// then group every job hitting the same fingerprint into one incident.
func TestSearchOffersCarriesProviderErrorWithFingerprint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := `#!/bin/sh
printf '%s\n' '{"error": true, "status_code": 400, "msg": " ask_contract_offers.driver_vers gte None: query values can'"'"'t be None"}' >&2
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	c := &Client{CLIPath: stub}
	_, err := c.SearchOffers(OfferConstraints{GPUClass: "nvidia", MinGPUMemGB: 10, NumGPUs: 1, MinDriverVersion: 535})
	if err == nil {
		t.Fatal("SearchOffers err = nil, want error")
	}
	if !errors.Is(err, cloud.ErrProviderRejected) {
		t.Fatalf("errors.Is(ErrProviderRejected) = false, err = %v", err)
	}
	pe, ok := cloud.AsProviderError(err)
	if !ok {
		t.Fatalf("AsProviderError returned ok=false, err = %v", err)
	}
	if pe.Fingerprint != "vastai/search-offers/400/bad-field:driver_vers" {
		t.Fatalf("Fingerprint = %q, want vastai/search-offers/400/bad-field:driver_vers", pe.Fingerprint)
	}
	if pe.StatusCode != 400 {
		t.Fatalf("StatusCode = %d, want 400", pe.StatusCode)
	}
}

// TestOperationPrefixDropsFlags verifies that the error-message prefix
// rendering drops flag tokens (anything starting with "-") so users see the
// semantic operation, not the literal command line.
func TestOperationPrefixDropsFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		max  int
		want []string
	}{
		{"empty", nil, 3, nil},
		{"all positional", []string{"show", "instance", "123"}, 3, []string{"show", "instance", "123"}},
		{"trailing flag dropped", []string{"search", "offers", "--raw"}, 3, []string{"search", "offers"}},
		{"interleaved flags dropped", []string{"destroy", "instance", "123", "-y", "--raw"}, 3, []string{"destroy", "instance", "123"}},
		{"cap respected", []string{"a", "b", "c", "d"}, 2, []string{"a", "b"}},
		{"flags at start dropped", []string{"--global", "show", "user"}, 3, []string{"show", "user"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := operationPrefix(tc.args, tc.max)
			if len(got) != len(tc.want) {
				t.Fatalf("operationPrefix(%v, %d) = %v, want %v", tc.args, tc.max, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("operationPrefix(%v, %d)[%d] = %q, want %q", tc.args, tc.max, i, got[i], tc.want[i])
				}
			}
		})
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
			// OfferConstraints carries the effective floor already resolved by
			// submission; the provider boundary must not add headroom again.
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

func TestBuildSearchFilter_HostRAM(t *testing.T) {
	// cpu_ram is reported in MB, so 32 GB -> cpu_ram>=32768.
	filter, _ := buildSearchFilter(OfferConstraints{MinHostRAMGB: 32})
	if !strings.Contains(filter, "cpu_ram>=32768") {
		t.Errorf("filter %q missing cpu_ram>=32768", filter)
	}
	if got, _ := buildSearchFilter(OfferConstraints{}); strings.Contains(got, "cpu_ram") {
		t.Errorf("filter %q should not constrain cpu_ram when MinHostRAMGB is 0", got)
	}
}

// TestBuildSearchFilter_HardwareCeilingDoesNotInflateMem is the regression
// for wj2265 and peers: when the request names a known hardware ceiling
// (A100 80GB) the search filter must emit `gpu_ram>=80`, not `>=82`, or
// every A100 80GB offer is excluded by the +2GB safety headroom. This
// covers both new submissions (stored 80, exact ceiling) and historical
// jobs persisted before the resolution moved to filter time (stored 82,
// rolled back to 80).
func TestBuildSearchFilter_HardwareCeilingDoesNotInflateMem(t *testing.T) {
	cases := []struct {
		name string
		c    OfferConstraints
		want string
	}{
		{"exact ceiling", OfferConstraints{GPUClass: "a100", MinGPUMemGB: 80}, "gpu_ram>=80"},
		{"post-headroom rollback", OfferConstraints{GPUClass: "a100", MinGPUMemGB: 82}, "gpu_ram>=80"},
		{"a100 pcie exact ceiling", OfferConstraints{GPUClass: "A100 PCIE", MinGPUMemGB: 80}, "gpu_ram>=80"},
		{"a100 pcie post-headroom rollback", OfferConstraints{GPUClass: "A100 PCIE", MinGPUMemGB: 82}, "gpu_ram>=80"},
		{"a100 sxm exact ceiling", OfferConstraints{GPUClass: "A100 SXM4", MinGPUMemGB: 80}, "gpu_ram>=80"},
		{"effective non-ceiling floor", OfferConstraints{GPUClass: "a100", MinGPUMemGB: 52}, "gpu_ram>=52"},
		{"unknown class effective floor", OfferConstraints{GPUClass: "nvidia", MinGPUMemGB: 26}, "gpu_ram>=26"},
		{"h100 80GB", OfferConstraints{GPUClass: "h100", MinGPUMemGB: 80}, "gpu_ram>=80"},
		{"h100 post-headroom", OfferConstraints{GPUClass: "h100", MinGPUMemGB: 82}, "gpu_ram>=80"},
		{"h100 pcie exact ceiling", OfferConstraints{GPUClass: "H100 PCIe", MinGPUMemGB: 80}, "gpu_ram>=80"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter, _ := buildSearchFilter(tc.c)
			if !strings.Contains(filter, tc.want) {
				t.Errorf("filter %q missing %q", filter, tc.want)
			}
		})
	}
}

// TestBuildSearchFilter_L4DoesNotDoubleHeadroom is the regression for wj6161
// and wj6187. Their explicit 20GB script reservation is persisted as an
// effective 22GB floor. Adding headroom again at the provider boundary searched
// for 24GB and excluded every L4 offer, whose provider-reported capacity is
// about 22.5GiB.
func TestBuildSearchFilter_L4DoesNotDoubleHeadroom(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{GPUClass: "l4", MinGPUMemGB: 22})
	if !strings.Contains(filter, "gpu_ram>=22") {
		t.Errorf("filter %q missing gpu_ram>=22", filter)
	}
	if strings.Contains(filter, "gpu_ram>=24") {
		t.Errorf("filter %q double-applies GPU memory headroom", filter)
	}
}

// TestBuildSearchFilter_MinDriverVersion asserts the dotted-version form.
//
// Regression: Vast.ai's `driver_version` filter is a string field that the CLI
// maps server-side to integer `driver_vers`. A bare integer ("driver_version>=535")
// fails the server's integer parse and returns 400
// "ask_contract_offers.driver_vers gte None: query values can't be None",
// silently blocking every offer search the autopilot makes. The CLI's own help
// example uses dotted form ("driver_version >= 535.86.05"); padding the major
// version as "<N>.00.00" yields a parseable value. Verified by hand against
// the live API on 2026-06-03 — see the commit message for the reproduction.
func TestBuildSearchFilter_MinDriverVersion(t *testing.T) {
	filter, _ := buildSearchFilter(OfferConstraints{MinDriverVersion: 535})
	if !strings.Contains(filter, "driver_version>=535.00.00") {
		t.Errorf("filter %q missing driver_version>=535.00.00 (dotted-version form)", filter)
	}
	// Bare integer form is the bug: must not regress to it.
	if strings.Contains(filter, "driver_version>=535 ") || strings.HasSuffix(filter, "driver_version>=535") {
		t.Errorf("filter %q emits bare integer driver_version — Vast.ai 400s on that form", filter)
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
	// MinGPUMemGB is already the effective floor. This test also asserts that
	// the deprecated maximum is dropped.
	filter, _ := buildSearchFilter(OfferConstraints{MinGPUMemGB: 24, MaxGPUMemGB: 48})
	if !strings.Contains(filter, "gpu_ram>=24") {
		t.Errorf("filter %q should contain gpu_ram>=24", filter)
	}
	if strings.Contains(filter, "gpu_ram<=") {
		t.Errorf("filter %q should not contain gpu_ram<= (MaxGPUMemGB must be ignored)", filter)
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

func TestChangeBidPassesPrice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\nprintf '{\"success\": true}\\n'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	c := &Client{CLIPath: stub}
	if err := c.ChangeBid(12345, 0.20); err != nil {
		t.Fatalf("ChangeBid: %v", err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	for _, want := range []string{"change\n", "bid\n", "12345\n", "--price\n", "0.2000\n"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("ChangeBid args missing %q; got:\n%s", want, got)
		}
	}
}

// Regression: when vastai reports "Instance N not found", treat destroy as
// idempotent success. Otherwise the reconciler (instance_check.go) defers
// terminal-status writes on every pass and a launch can wedge indefinitely.
func TestDestroyInstanceIdempotentWhenNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nprintf 'Instance 39145397 not found\\n' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	c := &Client{CLIPath: stub}
	if err := c.DestroyInstance(39145397); err != nil {
		t.Fatalf("DestroyInstance: expected nil for already-gone instance, got %v", err)
	}
}

// Negative case: confirm the not-found match is specific. An unrelated
// failure must still surface as an error so transient provider issues
// don't get silently swallowed.
func TestDestroyInstanceErrorsOnUnrelatedFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "vastai")
	script := "#!/bin/sh\nprintf 'Internal server error\\n' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	c := &Client{CLIPath: stub}
	if err := c.DestroyInstance(39145397); err == nil {
		t.Fatalf("DestroyInstance: expected error for unrelated failure, got nil")
	}
}
