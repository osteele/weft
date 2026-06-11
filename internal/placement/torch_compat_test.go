package placement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/inventory"
)

func TestTorchMaxComputeCap(t *testing.T) {
	cases := []struct {
		version string
		cuda    string
		want    string
	}{
		// Hopper-only era
		{"1.13.1", "cu117", "8.0"},
		{"2.0.1", "cu118", "9.0"},
		{"2.3.1", "cu121", "9.0"},
		{"2.4.1", "cu121", "9.0"},
		// 2.5: cu124 remains Hopper-bound; cu126 admits sm_100.
		{"2.5.0", "cu118", "9.0"},
		{"2.5.0", "cu121", "9.0"},
		{"2.5.1", "cu124", "9.0"},
		{"2.5.1", "cu126", "10.0"},
		// 2.6: sm_120 added on cu128
		{"2.6.0", "cu124", "9.0"},
		{"2.6.0", "cu126", "10.0"},
		{"2.6.0", "cu128", "12.0"},
		// 2.7+
		{"2.7.0", "cu126", "12.0"},
		{"2.7.0", "cu128", "12.0"},
		// CPU-only or unknown variant
		{"2.4.0", "cpu", ""},
		{"2.4.0", "", "9.0"},
		{"2.6.0", "", "9.0"},
		{"2.7.0", "", "12.0"},
		// Malformed version
		{"", "cu121", ""},
		{"abc", "cu121", ""},
		// Local-version suffix is stripped
		{"2.6.0+cu128", "cu128", "12.0"},
	}
	for _, c := range cases {
		got := TorchMaxComputeCap(c.version, c.cuda)
		if got != c.want {
			t.Errorf("TorchMaxComputeCap(%q, %q) = %q, want %q", c.version, c.cuda, got, c.want)
		}
	}
}

func TestTorchMinComputeCap(t *testing.T) {
	cases := []struct {
		version string
		cuda    string
		want    string
	}{
		{"2.7.0", "cu126", ""},
		{"2.7.0", "cu128", "7.5"},
		{"2.8.0", "", "7.5"},
		{"2.8.0", "cpu", ""},
		{"", "cu128", ""},
	}
	for _, c := range cases {
		got := TorchMinComputeCap(c.version, c.cuda)
		if got != c.want {
			t.Errorf("TorchMinComputeCap(%q, %q) = %q, want %q", c.version, c.cuda, got, c.want)
		}
	}
}

func TestComputeCapForGPU(t *testing.T) {
	cases := []struct {
		gpu  string
		want string
	}{
		{"A100", "8.0"},
		{"NVIDIA A100-PCIE-80GB", "8.0"},
		{"H100", "9.0"},
		{"RTX 3090", "8.6"},
		{"RTX 4090", "8.9"},
		{"B200", "10.0"},
		{"NVIDIA B200", "10.0"},
		{"RTX PRO 4500 Blackwell", "12.0"},
		{"RTX PRO 6000 WS", "12.0"},
		{"RTX 5090", "12.0"},
		// Regression for wj2365 on 2026-06-02: RunPod's offer.GPUName is
		// the terse displayName (no "Blackwell" suffix), so the
		// generation-name fallback misses these and we need explicit
		// catalog entries.
		{"RTX PRO 4500", "12.0"},
		{"RTX PRO 5000", "12.0"},
		{"RTX PRO 6000", "12.0"},
		// Pascal and Maxwell — regression for EXP-179 wj2240 on 2026-05-28,
		// where Vast.ai offered GTX 1080 Tis and weft's catalog didn't
		// recognize them, letting the torch-min compute-cap filter
		// fail-open and dispatching the job onto sm_61 (below torch 2.10's
		// sm_75 floor).
		{"GTX 1080 Ti", "6.1"},
		{"GTX 1080", "6.1"},
		{"GTX 1070", "6.1"},
		{"Tesla P100", "6.1"},
		{"TITAN Xp", "6.1"},
		{"Tesla M40", "5.2"},
		{"GTX 980 Ti", "5.2"},
		{"unknown-gpu-name", ""},
		{"", ""},
	}
	for _, c := range cases {
		got := ComputeCapForGPU(c.gpu)
		if got != c.want {
			t.Errorf("ComputeCapForGPU(%q) = %q, want %q", c.gpu, got, c.want)
		}
	}
}

func TestCompareComputeCap(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"9.0", "10.0", -1},
		{"10.0", "12.0", -1},
		{"12.0", "9.0", 1},
		{"9.0", "9.0", 0},
		{"", "9.0", -1},
		{"9.0", "", 1},
		{"", "", 0},
		{"garbage", "9.0", -1},
	}
	for _, c := range cases {
		got := CompareComputeCap(c.a, c.b)
		if got != c.want {
			t.Errorf("CompareComputeCap(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestArchNameToMaxCap(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"ampere", "8.6"},
		{"hopper", "9.0"},
		{"blackwell", "12.0"},
		{"any", ""},
		{"", ""},
		{"unknown-arch", ""},
		{"AMPERE", "8.6"},
	}
	for _, c := range cases {
		got := ArchNameToMaxCap(c.name)
		if got != c.want {
			t.Errorf("ArchNameToMaxCap(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMaxComputeCapForJob_Override(t *testing.T) {
	cases := []struct {
		archMax string
		want    string
	}{
		{"any", ""},
		{"hopper", "9.0"},
		{"9.0", "9.0"},
		{"12.0", "12.0"},
		{"unknown", ""},
	}
	for _, c := range cases {
		got := MaxComputeCapForJob(c.archMax, "")
		if got != c.want {
			t.Errorf("MaxComputeCapForJob(%q, \"\") = %q, want %q", c.archMax, got, c.want)
		}
	}
}

func TestResolveMaxComputeCapForPersistence(t *testing.T) {
	// dir="" means ScanTorchPin returns nil — represents a project with no
	// readable torch pin (pure CPU, missing source, etc.). The persisted-cap
	// resolver should record "any" rather than "" so downstream readers can
	// distinguish "explicitly unbounded" from "unresolved".
	cases := []struct {
		archMax string
		want    string
	}{
		{"any", "any"},
		{"hopper", "9.0"},
		{"9.0", "9.0"},
		{"", "any"}, // no override + no torch pin → explicit unbounded
	}
	for _, c := range cases {
		got := ResolveMaxComputeCapForPersistence(c.archMax, "")
		if got != c.want {
			t.Errorf("ResolveMaxComputeCapForPersistence(%q, \"\") = %q, want %q", c.archMax, got, c.want)
		}
	}
}

func writeTestUVLockCu128(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lock := `
[[package]]
name = "torch"
version = "2.9.1"
source = { registry = "https://pypi.org/simple" }

[[package]]
name = "nvidia-cuda-runtime-cu12"
version = "12.8.90"
`
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Regression for the wb18 driver-floor defect: a cu128 pip-wheel torch pin
// must imply the CUDA FAMILY floor (12.0 / driver >=525), not the 12.8
// toolkit floor (driver >=570) that excluded every on-prem host.
func TestMinRuntimeFloorForJob_TorchPinUsesFamilyFloor(t *testing.T) {
	dir := writeTestUVLockCu128(t)

	rf, err := MinRuntimeFloorForJob(dir, "uv run train.py")
	if err != nil {
		t.Fatalf("MinRuntimeFloorForJob: %v", err)
	}
	if rf.Req.MinCUDAVersion != "12.0" {
		t.Errorf("MinCUDAVersion = %q, want %q (family floor)", rf.Req.MinCUDAVersion, "12.0")
	}
	if rf.Req.MinDriverVersion != 525 {
		t.Errorf("MinDriverVersion = %d, want 525", rf.Req.MinDriverVersion)
	}
	if !strings.Contains(rf.CUDAOrigin, "torch 2.9.1+cu128") {
		t.Errorf("CUDAOrigin = %q, want torch pin provenance", rf.CUDAOrigin)
	}
}

// Library dependency floors are cited toolkit requirements and stay exact:
// vLLM >= 0.17 needs CUDA 12.8 / driver 570 regardless of the torch family
// floor.
func TestMinRuntimeFloorForJob_LibraryFloorStaysExact(t *testing.T) {
	dir := writeTestUVLockCu128(t)

	rf, err := MinRuntimeFloorForJob(dir, `uv run --with "vllm==0.17.0" serve.py`)
	if err != nil {
		t.Fatalf("MinRuntimeFloorForJob: %v", err)
	}
	if rf.Req.MinCUDAVersion != "12.8" {
		t.Errorf("MinCUDAVersion = %q, want %q (vLLM library floor)", rf.Req.MinCUDAVersion, "12.8")
	}
	if rf.Req.MinDriverVersion != 570 {
		t.Errorf("MinDriverVersion = %d, want 570", rf.Req.MinDriverVersion)
	}
	if !strings.Contains(rf.CUDAOrigin, "library") {
		t.Errorf("CUDAOrigin = %q, want library floor provenance", rf.CUDAOrigin)
	}
}

// End-to-end regression for wb18: hosts shaped like cool30 (driver 525.x,
// CUDA 12.0) and cool100 (driver 550.x, CUDA 12.4) must be ELIGIBLE for a
// GPU job from a cu128 torch-pin project; a genuinely old CUDA-11 host is
// still rejected.
func TestMinRuntimeFloor_OnPremHostsEligibleUnderCu128Pin(t *testing.T) {
	dir := writeTestUVLockCu128(t)
	rf, err := MinRuntimeFloorForJob(dir, "uv run train.py")
	if err != nil {
		t.Fatalf("MinRuntimeFloorForJob: %v", err)
	}
	c := Constraints{
		GPUClass:         "nvidia",
		GPUMemGB:         8,
		MinCUDAVersion:   rf.Req.MinCUDAVersion,
		MinDriverVersion: rf.Req.MinDriverVersion,
	}

	cool30 := inventory.HostSpec{
		Name:                "cool30-shaped",
		NVIDIADriverVersion: "525.125.06",
		CUDAVersion:         "12.0",
		GPUs:                []inventory.GPUSpec{{Name: "RTX 3090", Class: "rtx3090", Memory: "24GB"}},
	}
	cool100 := inventory.HostSpec{
		Name:                "cool100-shaped",
		NVIDIADriverVersion: "550.120",
		CUDAVersion:         "12.4",
		GPUs:                []inventory.GPUSpec{{Name: "A100 80GB PCIe", Class: "a100", Memory: "80GB"}},
	}
	old := inventory.HostSpec{
		Name:                "cuda11-host",
		NVIDIADriverVersion: "450.80.02",
		CUDAVersion:         "11.4",
		GPUs:                []inventory.GPUSpec{{Name: "RTX 3090", Class: "rtx3090", Memory: "24GB"}},
	}

	for _, host := range []inventory.HostSpec{cool30, cool100} {
		if ok, reasons := CheckHostGPUConstraints(host, c); !ok {
			t.Errorf("%s should be eligible under cu128 family floor: %v", host.Name, reasons)
		}
	}
	if ok, _ := CheckHostGPUConstraints(old, c); ok {
		t.Errorf("CUDA-11 host should be rejected under cu128 family floor")
	}
}
