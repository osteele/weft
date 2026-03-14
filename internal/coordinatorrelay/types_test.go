package coordinatorrelay

import (
	"encoding/json"
	"testing"
)

func TestRequestAckJSONRoundTrip(t *testing.T) {
	gpuMem := 24
	payload := Request{
		RequestID: "req-123",
		CreatedAt: "2026-03-15T12:00:00Z",
		Op:        OpSubmitJob,
		JobID:     77,
		Client: &ClientMetadata{
			Hostname: "laptop",
			PID:      1234,
			Version:  "test",
		},
		Submit: &SubmitJobPayload{
			Host:        "cool30",
			WorkingDir:  "/src/app",
			Command:     "python train.py",
			Description: "train",
			GPUMemGB:    &gpuMem,
			Inputs:      []string{"file:/tmp/config.json"},
		},
		Source: &SourceBundleRef{
			Kind: SourceRefR2,
			Hash: "abc123",
			Path: "sources/abc123.tar.gz",
			Entries: []SourceBundleEntry{
				{ID: "main", Kind: SourceEntryDir, RelPath: "entries/0", RemotePath: "/src/app", Delete: true},
			},
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var decoded Request
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if decoded.RequestID != payload.RequestID || decoded.JobID != payload.JobID || decoded.Op != payload.Op {
		t.Fatalf("decoded request mismatch: %+v", decoded)
	}
	if decoded.Source == nil || decoded.Source.Kind != SourceRefR2 || len(decoded.Source.Entries) != 1 {
		t.Fatalf("decoded source mismatch: %+v", decoded.Source)
	}
	if decoded.Submit == nil || decoded.Submit.Command != payload.Submit.Command {
		t.Fatalf("decoded submit mismatch: %+v", decoded.Submit)
	}

	ack := Ack{
		RequestID:   payload.RequestID,
		ProcessedAt: "2026-03-15T12:00:05Z",
		Accepted:    true,
		JobID:       payload.JobID,
		Host:        "cool30",
		Message:     "queued",
	}
	ackData, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal ack: %v", err)
	}

	var decodedAck Ack
	if err := json.Unmarshal(ackData, &decodedAck); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}
	if decodedAck != ack {
		t.Fatalf("decoded ack = %+v, want %+v", decodedAck, ack)
	}
}
