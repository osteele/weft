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

func TestNextArchiveSeqEmpty(t *testing.T) {
	logDir := t.TempDir()
	if got := nextArchiveSeq(logDir, 42); got != 1 {
		t.Fatalf("nextArchiveSeq(empty) = %d, want 1", got)
	}
}

func TestNextArchiveSeqSkipsTaken(t *testing.T) {
	logDir := t.TempDir()
	for _, ext := range []string{"log", "status"} {
		if err := os.WriteFile(filepath.Join(logDir, "42-1."+ext), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(logDir, "42-3.completion.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := nextArchiveSeq(logDir, 42); got != 2 {
		t.Fatalf("nextArchiveSeq with seq 1 and 3 taken = %d, want 2 (smallest free)", got)
	}
}

func TestArchiveExistingFilesUsesSequenceSuffix(t *testing.T) {
	logDir := t.TempDir()

	primary := filepath.Join(logDir, "100.log")
	if err := os.WriteFile(primary, []byte("first"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveExistingFiles(logDir, 100); err != nil {
		t.Fatalf("ArchiveExistingFiles: %v", err)
	}
	if _, err := os.Stat(primary); err == nil {
		t.Fatal("primary log should be moved")
	}
	first := filepath.Join(logDir, "100-1.log")
	if data, err := os.ReadFile(first); err != nil {
		t.Fatalf("expected archived %s: %v", first, err)
	} else if string(data) != "first" {
		t.Fatalf("archived content = %q, want \"first\"", data)
	}

	if err := os.WriteFile(primary, []byte("second"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveExistingFiles(logDir, 100); err != nil {
		t.Fatalf("ArchiveExistingFiles (second): %v", err)
	}
	second := filepath.Join(logDir, "100-2.log")
	if data, err := os.ReadFile(second); err != nil {
		t.Fatalf("expected archived %s: %v", second, err)
	} else if string(data) != "second" {
		t.Fatalf("second archived content = %q, want \"second\"", data)
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
