package runner

import (
	"encoding/json"
	"reflect"
	"testing"
)

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
