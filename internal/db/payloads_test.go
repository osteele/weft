package db

import (
	"strings"
	"testing"
)

func TestJobPayloadIsJobScopedAndCascades(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueuedWithGPU(database, "host-alpha", "/tmp/project", "true", "", "")
	if err != nil {
		t.Fatal(err)
	}
	payload := JobPayload{JobID: jobID, Name: "config", StoredPath: "payloads/hash", SizeBytes: 7, SHA256: strings.Repeat("a", 64), R2Key: "assets/hash"}
	if err := InsertJobPayload(database, payload); err != nil {
		t.Fatal(err)
	}
	if err := UpsertArtifact(database, Artifact{JobID: jobID, Name: "result", Path: "output/result", StoredPath: "x", SizeBytes: 1, SHA256: "b"}); err != nil {
		t.Fatal(err)
	}
	outputs, err := ListArtifactsByJob(database, jobID)
	if err != nil || len(outputs) != 1 || outputs[0].Path != "output/result" {
		t.Fatalf("output artifacts changed by payload row: %+v, %v", outputs, err)
	}
	if err := RequeueByID(database, jobID); err != nil {
		t.Fatal(err)
	}
	got, err := GetJobPayload(database, jobID, "config")
	if err != nil || got.SHA256 != payload.SHA256 {
		t.Fatalf("payload after retry = %+v, %v", got, err)
	}
	if _, err := database.Exec(`DELETE FROM job_attempts WHERE job_id = ?`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := DeleteJob(database, jobID); err != nil {
		t.Fatal(err)
	}
	if rows, err := ListJobPayloads(database, jobID); err != nil || len(rows) != 0 {
		t.Fatalf("payloads after delete = %+v, %v", rows, err)
	}
}
