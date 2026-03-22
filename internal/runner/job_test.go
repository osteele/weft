package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestJobCompleted(t *testing.T) {
	logDir := t.TempDir()

	// No status file → not completed
	if JobCompleted(logDir, 371) {
		t.Fatal("expected JobCompleted=false with no status file")
	}

	// Archived status file only → not completed (requeued job)
	archived := filepath.Join(logDir, "371-20260101-120000.status")
	if err := os.WriteFile(archived, []byte("0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if JobCompleted(logDir, 371) {
		t.Fatal("expected JobCompleted=false with only archived status file")
	}

	// Primary status file → completed
	primary := filepath.Join(logDir, "371.status")
	if err := os.WriteFile(primary, []byte("0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if !JobCompleted(logDir, 371) {
		t.Fatal("expected JobCompleted=true with primary status file")
	}
}

func TestCompletionRecordOutputUploadRoundTrip(t *testing.T) {
	rec := CompletionRecord{
		ExitCode:     0,
		WallTimeSecs: 1,
		EndTime:      2,
		OutputUpload: &OutputUploadResult{
			Status:          "partial",
			FileCount:       3,
			Bytes:           4096,
			RetryCount:      1,
			DurationMS:      1234,
			StartedAtUnix:   10,
			CompletedAtUnix: 12,
			Dirs: []OutputDirUpload{
				{
					Dir:        "output",
					Status:     "failed",
					Error:      "disk full",
					FileCount:  2,
					Bytes:      2048,
					RetryCount: 1,
					DurationMS: 1234,
				},
				{
					Dir:        "outputs",
					Status:     "ok",
					FileCount:  1,
					Bytes:      2048,
					DurationMS: 456,
				},
			},
		},
		ResultsUpload: &UploadSummary{
			Status:          "ok",
			FileCount:       4,
			Bytes:           8192,
			DurationMS:      567,
			StartedAtUnix:   12,
			CompletedAtUnix: 13,
		},
	}

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded CompletionRecord
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !reflect.DeepEqual(rec.OutputUpload, decoded.OutputUpload) {
		t.Fatalf("output upload mismatch: %#v != %#v", rec.OutputUpload, decoded.OutputUpload)
	}
	if !reflect.DeepEqual(rec.ResultsUpload, decoded.ResultsUpload) {
		t.Fatalf("results upload mismatch: %#v != %#v", rec.ResultsUpload, decoded.ResultsUpload)
	}
}
