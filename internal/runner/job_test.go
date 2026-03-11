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
			Status: "partial",
			Dirs: []OutputDirUpload{
				{
					Dir:        "output",
					Status:     "failed",
					Error:      "disk full",
					DurationMS: 1234,
				},
				{
					Dir:        "outputs",
					Status:     "ok",
					DurationMS: 456,
				},
			},
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
}
