package placement

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
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
	return writeTestUVLockCu128Version(t, "2.9.1")
}

func writeTestUVLockCu128Version(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	dataloc.WriteTestTorchPin(t, dir, version, "cu128")
	return dir
}

// Regression for the wb18 driver-floor defect: older cu128 pip-wheel torch
// pins still use the CUDA FAMILY floor (12.0 / driver >=525), not the 12.8
// toolkit floor (driver >=570) that excluded every on-prem host.
func TestMinRuntimeFloorForJob_TorchPinUsesFamilyFloor(t *testing.T) {
	dir := writeTestUVLockCu128Version(t, "2.6.0")

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
	if !strings.Contains(rf.CUDAOrigin, "torch 2.6.0+cu128") {
		t.Errorf("CUDAOrigin = %q, want torch pin provenance", rf.CUDAOrigin)
	}
}

// Regression for wb32: torch 2.9.x cu128 is known to fail on cool30's
// 525/CUDA-12.0 driver, so placement needs an operational floor above the
// theoretical CUDA-family floor.
func TestMinRuntimeFloorForJob_TorchCu128OperationalFloor(t *testing.T) {
	dir := writeTestUVLockCu128(t)

	rf, err := MinRuntimeFloorForJob(dir, "uv run train.py")
	if err != nil {
		t.Fatalf("MinRuntimeFloorForJob: %v", err)
	}
	if rf.Req.MinCUDAVersion != "12.8" {
		t.Errorf("MinCUDAVersion = %q, want 12.8 (operational floor)", rf.Req.MinCUDAVersion)
	}
	if rf.Req.MinDriverVersion != 570 {
		t.Errorf("MinDriverVersion = %d, want 570", rf.Req.MinDriverVersion)
	}
	if !strings.Contains(rf.CUDAOrigin, "operational floor") {
		t.Errorf("CUDAOrigin = %q, want operational floor provenance", rf.CUDAOrigin)
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

func TestMinRuntimeFloorForJob_ScriptTorchRangeOverridesProjectLock(t *testing.T) {
	dir := writeTestUVLockCu128Version(t, "2.6.0")
	script := `# /// script
# dependencies = ["torch>=2.2", "transformers>=4.44", "numpy>=1.26"]
# ///
import torch
`
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write train.py: %v", err)
	}

	rf, err := MinRuntimeFloorForJob(dir, "uv run train.py")
	if err != nil {
		t.Fatalf("MinRuntimeFloorForJob: %v", err)
	}
	if rf.Req.MinCUDAVersion != "13.0" {
		t.Errorf("MinCUDAVersion = %q, want 13.0 for open script torch range", rf.Req.MinCUDAVersion)
	}
	if rf.Req.MinDriverVersion != 580 {
		t.Errorf("MinDriverVersion = %d, want 580", rf.Req.MinDriverVersion)
	}
	if !strings.Contains(rf.CUDAOrigin, "script PEP 723 torch") {
		t.Errorf("CUDAOrigin = %q, want script torch provenance", rf.CUDAOrigin)
	}
}

// End-to-end regression for wb18: older cu128 pins should still admit hosts
// shaped like cool30 (driver 525.x, CUDA 12.0) and cool100 (driver 550.x,
// CUDA 12.4); a genuinely old CUDA-11 host is still rejected.
func TestMinRuntimeFloor_OnPremHostsEligibleUnderCu128Pin(t *testing.T) {
	dir := writeTestUVLockCu128Version(t, "2.6.0")
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

func TestMinRuntimeFloor_Torch291Cu128RejectsCool30(t *testing.T) {
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
	newerDriver := inventory.HostSpec{
		Name:                "cuda128-host",
		NVIDIADriverVersion: "570.86.15",
		CUDAVersion:         "12.8",
		GPUs:                []inventory.GPUSpec{{Name: "A100 80GB PCIe", Class: "a100", Memory: "80GB"}},
	}

	for _, host := range []inventory.HostSpec{cool30, cool100} {
		if ok, reasons := CheckHostGPUConstraints(host, c); ok {
			t.Errorf("%s should be rejected under torch 2.9.1 cu128 operational floor: %v", host.Name, reasons)
		}
	}
	if ok, reasons := CheckHostGPUConstraints(newerDriver, c); !ok {
		t.Errorf("newer driver host should be eligible under torch 2.9.1 cu128 operational floor: %v", reasons)
	}
}

func writeScriptWithCUDADriverMin(t *testing.T, dir, value string) string {
	t.Helper()
	script := fmt.Sprintf(`# /// script
# dependencies = []
#
# [tool.weft]
# cuda-driver-min = %q
# ///
print("hi")
`, value)
	path := filepath.Join(dir, "train.py")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Regression for wb18: cuda-driver-min = "cu128" was silently discarded by a
// float parse whose error was swallowed. The cuNNN spelling must parse, and
// an explicit value must REPLACE the inferred floor provenance rather than be
// dropped.
func TestMinRuntimeFloorForJob_CuNNNSpellingParses(t *testing.T) {
	dir := writeTestUVLockCu128(t)
	writeScriptWithCUDADriverMin(t, dir, "cu128")

	rf, err := MinRuntimeFloorForJob(dir, "uv run train.py")
	if err != nil {
		t.Fatalf("MinRuntimeFloorForJob: %v", err)
	}
	if rf.Req.MinCUDAVersion != "12.8" {
		t.Errorf("MinCUDAVersion = %q, want %q (explicit cu128)", rf.Req.MinCUDAVersion, "12.8")
	}
	if rf.Req.MinDriverVersion != 570 {
		t.Errorf("MinDriverVersion = %d, want 570", rf.Req.MinDriverVersion)
	}
	if !strings.Contains(rf.CUDAOrigin, "script [tool.weft]") {
		t.Errorf("CUDAOrigin = %q, want script provenance", rf.CUDAOrigin)
	}
}

// An explicit cuda-driver-min may LOWER the inferred floor: a library floor
// of 13.0 drops to 12.0 / driver 525 when the user pins it down.
func TestMinRuntimeFloorForJob_ExplicitLowersInferredFloor(t *testing.T) {
	dir := writeTestUVLockCu128(t)
	writeScriptWithCUDADriverMin(t, dir, "12.0")

	rf, err := MinRuntimeFloorForJob(dir, `uv run --with "vllm>=0.20" train.py`)
	if err != nil {
		t.Fatalf("MinRuntimeFloorForJob: %v", err)
	}
	if rf.Req.MinCUDAVersion != "12.0" {
		t.Errorf("MinCUDAVersion = %q, want %q (explicit lowers library floor)", rf.Req.MinCUDAVersion, "12.0")
	}
	if rf.Req.MinDriverVersion != 525 {
		t.Errorf("MinDriverVersion = %d, want 525", rf.Req.MinDriverVersion)
	}
}

// cuda-driver-min = "any" clears the inferred CUDA and driver floors.
func TestMinRuntimeFloorForJob_AnyClearsFloors(t *testing.T) {
	dir := writeTestUVLockCu128(t)
	writeScriptWithCUDADriverMin(t, dir, "any")

	rf, err := MinRuntimeFloorForJob(dir, "uv run train.py")
	if err != nil {
		t.Fatalf("MinRuntimeFloorForJob: %v", err)
	}
	if rf.Req.MinCUDAVersion != "" || rf.Req.MinDriverVersion != 0 {
		t.Errorf("floors = %q/%d, want cleared", rf.Req.MinCUDAVersion, rf.Req.MinDriverVersion)
	}
}

// Unparseable explicit metadata returns an error (surfaced at submit time)
// while the floor still reflects the sources that parsed.
func TestMinRuntimeFloorForJob_GarbageErrors(t *testing.T) {
	dir := writeTestUVLockCu128(t)
	writeScriptWithCUDADriverMin(t, dir, "garbage")

	rf, err := MinRuntimeFloorForJob(dir, "uv run train.py")
	if err == nil {
		t.Fatal("expected parse error for cuda-driver-min = garbage")
	}
	if rf.Req.MinCUDAVersion != "12.8" {
		t.Errorf("MinCUDAVersion = %q, want operational floor retained", rf.Req.MinCUDAVersion)
	}
}

// An explicit min-driver is preserved exactly — the CUDA-floor backfill must
// not raise it.
func TestRuntimeFloor_ExplicitDriverNotRaised(t *testing.T) {
	var rf RuntimeFloor
	rf.MergeInferred(cloud.ImageRequirements{MinCUDAVersion: "12.8"}, "torch pin")
	if err := rf.ApplyExplicit("530", "", "script [tool.weft]"); err != nil {
		t.Fatalf("ApplyExplicit: %v", err)
	}
	rf.FinalizeDriver()
	if rf.Req.MinDriverVersion != 530 {
		t.Errorf("MinDriverVersion = %d, want explicit 530 preserved", rf.Req.MinDriverVersion)
	}
}
