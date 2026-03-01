package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteTimeseriesSample(t *testing.T) {
	dir := t.TempDir()
	paths := NewJobPaths(dir, 123)

	sample1 := TimeseriesSample{
		Ts:     1704067200,
		CPUPct: 25,
		RSSKB:  400000,
		GPUMiB: 6000,
		Tenant: "multi",
	}
	sample2 := TimeseriesSample{
		Ts:           1704067215,
		CPUPct:       28,
		RSSKB:        500000,
		GPUMiB:       8000,
		HostRSSKB:    1000000,
		HostMemTotal: 16000000,
		GPUUtilPct:   85,
		GPUMemUsed:   12000,
		GPUMemTotal:  24000,
		Tenant:       "single",
	}

	if err := WriteTimeseriesSample(paths, sample1); err != nil {
		t.Fatalf("write sample 1: %v", err)
	}
	if err := WriteTimeseriesSample(paths, sample2); err != nil {
		t.Fatalf("write sample 2: %v", err)
	}

	// Verify file exists at expected path
	expectedPath := filepath.Join(dir, "123.timeseries.jsonl")
	if paths.Timeseries != expectedPath {
		t.Errorf("expected path %s, got %s", expectedPath, paths.Timeseries)
	}

	data, err := os.ReadFile(paths.Timeseries)
	if err != nil {
		t.Fatalf("read timeseries file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}

	// Parse first line
	var parsed1 TimeseriesSample
	if err := json.Unmarshal([]byte(lines[0]), &parsed1); err != nil {
		t.Fatalf("parse line 1: %v", err)
	}
	if parsed1.Ts != 1704067200 || parsed1.CPUPct != 25 || parsed1.RSSKB != 400000 {
		t.Errorf("sample 1 mismatch: %+v", parsed1)
	}

	// Parse second line
	var parsed2 TimeseriesSample
	if err := json.Unmarshal([]byte(lines[1]), &parsed2); err != nil {
		t.Fatalf("parse line 2: %v", err)
	}
	if parsed2.GPUUtilPct != 85 || parsed2.GPUMemUsed != 12000 {
		t.Errorf("sample 2 GPU metrics mismatch: %+v", parsed2)
	}
	if parsed2.Tenant != "single" {
		t.Errorf("expected tenant=single, got %q", parsed2.Tenant)
	}
}

func TestNewJobPaths_IncludesTimeseries(t *testing.T) {
	paths := NewJobPaths("/tmp/logs", 456)
	expected := "/tmp/logs/456.timeseries.jsonl"
	if paths.Timeseries != expected {
		t.Errorf("expected %s, got %s", expected, paths.Timeseries)
	}
}
