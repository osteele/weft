package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/osteele/weft/internal/runner"
)

func TestPatchCompletionUpload(t *testing.T) {
	logDir := t.TempDir()
	jobID := int64(42)
	paths := runner.NewJobPaths(logDir, jobID)

	initial := runner.CompletionRecord{
		ExitCode:     0,
		WallTimeSecs: 1,
		EndTime:      2,
	}
	data, err := json.MarshalIndent(initial, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(paths.Completion, data, 0644); err != nil {
		t.Fatalf("write completion: %v", err)
	}

	upload := runner.OutputUploadResult{
		Status: "failed",
		Dirs: []runner.OutputDirUpload{
			{
				Dir:        "output",
				Status:     "failed",
				Error:      "disk full",
				DurationMS: 1200,
			},
		},
	}

	patchCompletionUpload(logDir, jobID, &upload)

	updated, err := os.ReadFile(paths.Completion)
	if err != nil {
		t.Fatalf("read completion: %v", err)
	}
	var decoded runner.CompletionRecord
	if err := json.Unmarshal(updated, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded.OutputUpload, &upload) {
		t.Fatalf("output upload mismatch: %#v != %#v", decoded.OutputUpload, upload)
	}
}
