package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCollectHeartbeat(t *testing.T) {
	sample := collectHeartbeat("running:42")

	if sample.Phase != "running:42" {
		t.Errorf("Phase = %q, want %q", sample.Phase, "running:42")
	}

	if sample.Ts == 0 {
		t.Error("Ts should be non-zero")
	}

	now := time.Now().Unix()
	if sample.Ts < now-5 || sample.Ts > now+5 {
		t.Errorf("Ts = %d, want within 5s of %d", sample.Ts, now)
	}

	// DiskTotalBytes should be populated on any OS
	if sample.DiskTotalBytes == 0 {
		t.Error("DiskTotalBytes should be non-zero")
	}
	if sample.DiskFreeBytes < 0 {
		t.Errorf("DiskFreeBytes = %d, should be >= 0", sample.DiskFreeBytes)
	}

	// HostMemTotalKB should be populated on Linux and macOS
	if sample.HostMemTotalKB == 0 {
		t.Error("HostMemTotalKB should be non-zero")
	}
}

func TestHeartbeatSampleJSON(t *testing.T) {
	sample := HeartbeatSample{
		Ts:             1710000000,
		Phase:          "running:149",
		GPUUtilPct:     85,
		GPUMemUsedMiB:  18200,
		GPUMemTotalMiB: 24576,
		GPUTempC:       72,
		HostRSSKB:      12000000,
		HostMemTotalKB: 64000000,
		DiskFreeBytes:  45000000000,
		DiskTotalBytes: 200000000000,
	}

	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded HeartbeatSample
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded != sample {
		t.Errorf("roundtrip mismatch:\n  got  %+v\n  want %+v", decoded, sample)
	}

	// Verify JSON field names
	var raw map[string]any
	json.Unmarshal(data, &raw)
	for _, key := range []string{"ts", "phase", "gpu_util_pct", "gpu_mem_used_mib", "gpu_temp_c", "disk_free_bytes"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("JSON missing expected key %q", key)
		}
	}
}
