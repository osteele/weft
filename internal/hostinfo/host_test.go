package hostinfo

import (
	"strings"
	"testing"
	"time"
)

func TestUpdateFrom_PreservesExistingFields(t *testing.T) {
	existing := &Host{
		Name:              "host1",
		Status:            HostStatusOnline,
		Arch:              "Linux x86_64",
		CPUs:              8,
		MemTotal:          "128G",
		MemUsed:           "64G",
		LoadAvg:           "1.5, 1.0, 0.5",
		DiskFree:          100 * 1024 * 1024,
		DiskTotal:         500 * 1024 * 1024,
		QueueStatus:       QueueCheckChecked,
		QueueRunnerActive: true,
		QueuedJobCount:    3,
		RunningJobs:       []HostRunningJob{{ID: 42}},
		GPUs:              []GPUInfo{{Index: 0, Name: "A100"}},
	}

	// Source with only some fields set
	source := &Host{
		Status:  HostStatusOnline,
		MemUsed: "70G",
		LoadAvg: "2.0, 1.5, 1.0",
	}

	existing.UpdateFrom(source)

	// Updated fields
	if existing.MemUsed != "70G" {
		t.Errorf("MemUsed = %q, want %q", existing.MemUsed, "70G")
	}
	if existing.LoadAvg != "2.0, 1.5, 1.0" {
		t.Errorf("LoadAvg = %q, want %q", existing.LoadAvg, "2.0, 1.5, 1.0")
	}

	// Preserved fields (source was zero-valued)
	if existing.Name != "host1" {
		t.Errorf("Name = %q, want %q", existing.Name, "host1")
	}
	if existing.Arch != "Linux x86_64" {
		t.Errorf("Arch = %q, want %q", existing.Arch, "Linux x86_64")
	}
	if existing.CPUs != 8 {
		t.Errorf("CPUs = %d, want %d", existing.CPUs, 8)
	}
	if existing.MemTotal != "128G" {
		t.Errorf("MemTotal = %q, want %q", existing.MemTotal, "128G")
	}
	if existing.DiskFree != 100*1024*1024 {
		t.Errorf("DiskFree not preserved")
	}
	if existing.QueueStatus != QueueCheckChecked {
		t.Errorf("QueueStatus not preserved")
	}
	if existing.QueueRunnerActive != true {
		t.Errorf("QueueRunnerActive not preserved")
	}
	if existing.QueuedJobCount != 3 {
		t.Errorf("QueuedJobCount not preserved")
	}
	if len(existing.RunningJobs) != 1 || existing.RunningJobs[0].ID != 42 {
		t.Errorf("RunningJobs not preserved")
	}
	if len(existing.GPUs) != 1 || existing.GPUs[0].Name != "A100" {
		t.Errorf("GPUs not preserved")
	}
}

func TestUpdateFrom_OverwritesAllFields(t *testing.T) {
	existing := &Host{
		Name:   "host1",
		Status: HostStatusChecking,
	}

	now := time.Now()
	source := &Host{
		Status:              HostStatusOnline,
		Arch:                "Linux x86_64",
		OS:                  "5.15.0",
		OSRelease:           "ubuntu:22.04:Ubuntu 22.04.5 LTS",
		Model:               "server",
		CPUs:                16,
		CPUModel:            "Xeon",
		CPUFreq:             "3.2 GHz",
		MemTotal:            "256G",
		MemUsed:             "100G",
		LoadAvg:             "4.0, 3.0, 2.0",
		DiskFree:            200 * 1024 * 1024,
		DiskTotal:           1000 * 1024 * 1024,
		GPUs:                []GPUInfo{{Index: 0, Name: "V100"}},
		NVIDIADriverVersion: "550.120",
		CUDAVersion:         "12.4",
		GLIBCVersion:        "2.35",
		GLIBCXXMaxVersion:   "3.4.30",
		LastCheck:           now,
		QueueStatus:         QueueCheckChecked,
		QueueRunnerActive:   true,
		QueuedJobCount:      5,
		CurrentQueueJob:     "123",
		RunningJobs:         []HostRunningJob{{ID: 1}, {ID: 2}},
	}

	existing.UpdateFrom(source)

	if existing.Name != "host1" {
		t.Errorf("Name should not be overwritten")
	}
	if existing.Status != HostStatusOnline {
		t.Errorf("Status not updated")
	}
	if existing.CPUs != 16 {
		t.Errorf("CPUs = %d, want 16", existing.CPUs)
	}
	if len(existing.GPUs) != 1 || existing.GPUs[0].Name != "V100" {
		t.Errorf("GPUs not updated")
	}
	if len(existing.RunningJobs) != 2 {
		t.Errorf("RunningJobs not updated")
	}
	if existing.OSRelease != "ubuntu:22.04:Ubuntu 22.04.5 LTS" || existing.GLIBCXXMaxVersion != "3.4.30" {
		t.Errorf("toolchain facts not updated: os_release=%q glibcxx=%q", existing.OSRelease, existing.GLIBCXXMaxVersion)
	}
}

func TestParseHostInfo_ToolchainFacts(t *testing.T) {
	output := strings.Join([]string{
		"ARCH:Linux x86_64",
		"OS:5.15.0-generic",
		"OSRELEASE:ubuntu:20.04:Ubuntu 20.04.6 LTS",
		"GLIBC:2.31",
		"GLIBCXX:3.4.28",
		"",
	}, "\n")
	host := ParseHostInfo(output)
	if host.OSRelease != "ubuntu:20.04:Ubuntu 20.04.6 LTS" {
		t.Fatalf("OSRelease = %q", host.OSRelease)
	}
	if host.GLIBCVersion != "2.31" || host.GLIBCXXMaxVersion != "3.4.28" {
		t.Fatalf("toolchain = glibc %q glibcxx %q", host.GLIBCVersion, host.GLIBCXXMaxVersion)
	}
}

func TestUpdateFrom_ClearsErrorOnOnline(t *testing.T) {
	existing := &Host{
		Name:   "host1",
		Status: HostStatusOffline,
		Error:  "connection refused",
	}

	source := &Host{
		Status: HostStatusOnline,
	}

	existing.UpdateFrom(source)

	if existing.Error != "" {
		t.Errorf("Error should be cleared when status is online, got %q", existing.Error)
	}
}

func TestUpdateFrom_NilSafety(t *testing.T) {
	var h *Host
	h.UpdateFrom(&Host{Status: HostStatusOnline}) // should not panic

	h2 := &Host{Name: "test"}
	h2.UpdateFrom(nil) // should not panic
}

func TestParseGPUListLine(t *testing.T) {
	tests := []struct {
		line     string
		wantIdx  int
		wantName string
	}{
		{"GPU 0: NVIDIA GeForce RTX 3090 (UUID: GPU-abcdef12-3456-7890)", 0, "NVIDIA GeForce RTX 3090"},
		{"GPU 1: NVIDIA A100-PCIE-80GB (UUID: GPU-12345678-abcd-efgh)", 1, "NVIDIA A100-PCIE-80GB"},
		{"GPU 2: NVIDIA GeForce RTX 2080 Ti (UUID: GPU-deadbeef)", 2, "NVIDIA GeForce RTX 2080 Ti"},
		{"not a gpu line", -1, ""},
		{"GPU x: bad index", -1, ""},
	}
	for _, tt := range tests {
		idx, name := parseGPUListLine(tt.line)
		if idx != tt.wantIdx || name != tt.wantName {
			t.Errorf("parseGPUListLine(%q) = (%d, %q), want (%d, %q)",
				tt.line, idx, name, tt.wantIdx, tt.wantName)
		}
	}
}

func TestParseHostInfo_GPUListBackfillsTruncatedNames(t *testing.T) {
	// Simulate old driver (525.x) that truncates names in table output
	// but nvidia-smi -L gives full names
	output := `ARCH:Linux x86_64
OS:5.4.0
CPUS:96
MEM:503Gi:120Gi
GPUNAME:|   0  NVIDIA GeForce ...  On   | 00000000:01:00.0 Off |                  N/A |
GPUSTAT:| 51%   45C    P8    22W / 350W |      6MiB / 24576MiB |      0%      Default |
GPUNAME:|   1  NVIDIA GeForce ...  On   | 00000000:41:00.0 Off |                  N/A |
GPUSTAT:| 51%   45C    P8    19W / 350W |      6MiB / 24576MiB |      0%      Default |
GPUDRIVER:550.120
GPUCUDA:12.4
GPULIST:GPU 0: NVIDIA GeForce RTX 3090 (UUID: GPU-aaaa-bbbb)
GPULIST:GPU 1: NVIDIA GeForce RTX 3090 (UUID: GPU-cccc-dddd)`

	host := ParseHostInfo(output)

	if len(host.GPUs) != 2 {
		t.Fatalf("got %d GPUs, want 2", len(host.GPUs))
	}
	for i, gpu := range host.GPUs {
		if gpu.Name != "NVIDIA GeForce RTX 3090" {
			t.Errorf("GPU[%d].Name = %q, want %q", i, gpu.Name, "NVIDIA GeForce RTX 3090")
		}
	}
	if host.NVIDIADriverVersion != "550.120" {
		t.Errorf("NVIDIADriverVersion = %q, want 550.120", host.NVIDIADriverVersion)
	}
	if host.CUDAVersion != "12.4" {
		t.Errorf("CUDAVersion = %q, want 12.4", host.CUDAVersion)
	}
}

func TestParseHostInfo_GPUListNoBackfillWhenNotTruncated(t *testing.T) {
	// When table output already has full names, GPULIST should not overwrite
	output := `ARCH:Linux x86_64
CPUS:8
GPUNAME:|   0  NVIDIA A100-PCIE-80GB  On   | 00000000:01:00.0 Off |                  N/A |
GPUSTAT:| 30%   45C    P8    20W / 300W |    123MiB / 81920MiB |      0%      Default |
GPULIST:GPU 0: NVIDIA A100-PCIE-80GB (UUID: GPU-1234)`

	host := ParseHostInfo(output)

	if len(host.GPUs) != 1 {
		t.Fatalf("got %d GPUs, want 1", len(host.GPUs))
	}
	if host.GPUs[0].Name != "NVIDIA A100-PCIE-80GB" {
		t.Errorf("GPU[0].Name = %q, want %q", host.GPUs[0].Name, "NVIDIA A100-PCIE-80GB")
	}
}
