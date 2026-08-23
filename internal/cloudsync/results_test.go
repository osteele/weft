package cloudsync

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/logcache"
)

func TestWriteVastaiLogsToCache_AgentFormat(t *testing.T) {
	tmpDir := t.TempDir()
	// Redirect HOME so logcache writes to a temp location
	t.Setenv("HOME", tmpDir)

	jobID := int64(999)
	logContent := "epoch 1/10 loss=0.5\nepoch 2/10 loss=0.3\n"

	// Create agent-format log file: {jobID}.log
	resultsDir := t.TempDir()
	logPath := filepath.Join(resultsDir, fmt.Sprintf("%d.log", jobID))
	if err := os.WriteFile(logPath, []byte(logContent), 0644); err != nil {
		t.Fatal(err)
	}

	WriteVastaiLogsToCache(jobID, resultsDir)

	// Verify the log was cached
	cached, err := logcache.Read(jobID)
	if err != nil {
		t.Fatalf("logcache.Read() error: %v", err)
	}
	if cached != logContent {
		t.Errorf("cached content = %q, want %q", cached, logContent)
	}
	if !logcache.IsComplete(jobID) {
		t.Error("expected log to be marked as complete")
	}
}

func TestWriteVastaiLogsToCache_LegacyFormat(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	jobID := int64(1000)
	resultsDir := t.TempDir()

	// Create legacy format files
	os.WriteFile(filepath.Join(resultsDir, "stdout.log"), []byte("stdout output\n"), 0644)
	os.WriteFile(filepath.Join(resultsDir, "stderr.log"), []byte("stderr output\n"), 0644)

	WriteVastaiLogsToCache(jobID, resultsDir)

	cached, err := logcache.Read(jobID)
	if err != nil {
		t.Fatalf("logcache.Read() error: %v", err)
	}
	if !strings.Contains(cached, "stdout output") {
		t.Error("cached log should contain stdout")
	}
	if !strings.Contains(cached, "stderr output") {
		t.Error("cached log should contain stderr")
	}
}

func TestReadSummedInt64(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *int64
	}{
		{"single value", "42\n", int64Ptr(42)},
		{"multiple values", "10\n20\n12\n", int64Ptr(42)},
		{"trailing newline", "5\n", int64Ptr(5)},
		{"no trailing newline", "7", int64Ptr(7)},
		{"empty file", "", nil},
		{"whitespace only", "  \n  \n", nil},
		{"mixed valid and invalid", "10\nbad\n20\n", int64Ptr(30)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			os.WriteFile(path, []byte(tt.content), 0644)
			got := readSummedInt64(path)
			if tt.want == nil {
				if got != nil {
					t.Errorf("got %d, want nil", *got)
				}
			} else if got == nil {
				t.Errorf("got nil, want %d", *tt.want)
			} else if *got != *tt.want {
				t.Errorf("got %d, want %d", *got, *tt.want)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		got := readSummedInt64(filepath.Join(t.TempDir(), "nonexistent"))
		if got != nil {
			t.Errorf("got %d, want nil", *got)
		}
	})
}

func int64Ptr(v int64) *int64 { return &v }

func TestExtractPhaseTimings_NewFields(t *testing.T) {
	tmpDir := t.TempDir()
	jobID := int64(42)

	// Write phase files so ExtractPhaseTimings returns non-nil
	os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("phase_run_start_%d", jobID)), []byte("1000"), 0644)
	os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("phase_run_end_%d", jobID)), []byte("2000"), 0644)

	// Write uv sync timing (multiple invocations)
	os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("uv_sync_seconds_%d", jobID)), []byte("15\n8\n"), 0644)

	// Write post-job cache sizes
	os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("cache_uv_post_%d", jobID)), []byte("123456"), 0644)
	os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("cache_hf_post_%d", jobID)), []byte("789012"), 0644)

	timings := ExtractPhaseTimings(jobID, tmpDir)
	if timings == nil {
		t.Fatal("expected non-nil timings")
	}

	if timings.UVSyncSeconds == nil || *timings.UVSyncSeconds != 23 {
		t.Errorf("UVSyncSeconds = %v, want 23", timings.UVSyncSeconds)
	}
	if timings.CacheUVPostBytes == nil || *timings.CacheUVPostBytes != 123456 {
		t.Errorf("CacheUVPostBytes = %v, want 123456", timings.CacheUVPostBytes)
	}
	if timings.CacheHFPostBytes == nil || *timings.CacheHFPostBytes != 789012 {
		t.Errorf("CacheHFPostBytes = %v, want 789012", timings.CacheHFPostBytes)
	}
}

func TestExtractPhaseTimings_StructuredUploadMetrics(t *testing.T) {
	tmpDir := t.TempDir()
	jobID := int64(42)

	phases := map[string]any{
		"wrapper_start": 1000,
		"setup_start":   1005,
		"setup_end":     1010,
		"run_start":     1010,
		"run_end":       1020,
		"upload_start":  1021,
		"upload_end":    1024,
	}
	phaseData, err := json.Marshal(phases)
	if err != nil {
		t.Fatalf("marshal phases: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("%d.phases.json", jobID)), phaseData, 0644); err != nil {
		t.Fatalf("write phases: %v", err)
	}

	completion := map[string]any{
		"max_gpu_mem_mib": 2048,
		"output_upload": map[string]any{
			"bytes":       4096,
			"file_count":  3,
			"retry_count": 1,
			"duration_ms": 250,
		},
		"results_upload": map[string]any{
			"bytes":       8192,
			"file_count":  4,
			"duration_ms": 600,
		},
	}
	completionData, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("marshal completion: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("%d.completion.json", jobID)), completionData, 0644); err != nil {
		t.Fatalf("write completion: %v", err)
	}

	timings := ExtractPhaseTimings(jobID, tmpDir)
	if timings == nil {
		t.Fatal("expected non-nil timings")
	}
	if timings.UploadStart == nil || *timings.UploadStart != 1021 {
		t.Fatalf("UploadStart = %v, want 1021", timings.UploadStart)
	}
	if timings.UploadEnd == nil || *timings.UploadEnd != 1024 {
		t.Fatalf("UploadEnd = %v, want 1024", timings.UploadEnd)
	}
	if timings.UploadWorkspaceBytes == nil || *timings.UploadWorkspaceBytes != 4096 {
		t.Fatalf("UploadWorkspaceBytes = %v, want 4096", timings.UploadWorkspaceBytes)
	}
	if timings.OutputUploadFiles == nil || *timings.OutputUploadFiles != 3 {
		t.Fatalf("OutputUploadFiles = %v, want 3", timings.OutputUploadFiles)
	}
	if timings.OutputUploadRetries == nil || *timings.OutputUploadRetries != 1 {
		t.Fatalf("OutputUploadRetries = %v, want 1", timings.OutputUploadRetries)
	}
	if timings.ResultsUploadFiles == nil || *timings.ResultsUploadFiles != 4 {
		t.Fatalf("ResultsUploadFiles = %v, want 4", timings.ResultsUploadFiles)
	}
	if timings.UploadResultsBytes == nil || *timings.UploadResultsBytes != 8192 {
		t.Fatalf("UploadResultsBytes = %v, want 8192", timings.UploadResultsBytes)
	}
}
