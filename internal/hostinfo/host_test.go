package hostinfo

import (
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
		Status:            HostStatusOnline,
		Arch:              "Linux x86_64",
		OS:                "5.15.0",
		Model:             "server",
		CPUs:              16,
		CPUModel:          "Xeon",
		CPUFreq:           "3.2 GHz",
		MemTotal:          "256G",
		MemUsed:           "100G",
		LoadAvg:           "4.0, 3.0, 2.0",
		DiskFree:          200 * 1024 * 1024,
		DiskTotal:         1000 * 1024 * 1024,
		GPUs:              []GPUInfo{{Index: 0, Name: "V100"}},
		LastCheck:         now,
		QueueStatus:       QueueCheckChecked,
		QueueRunnerActive: true,
		QueuedJobCount:    5,
		CurrentQueueJob:   "123",
		RunningJobs:       []HostRunningJob{{ID: 1}, {ID: 2}},
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
