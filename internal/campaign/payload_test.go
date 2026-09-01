package campaign

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestAttachAgentJobPayloadsPreservesIdentity(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "true", "", "")
	if err != nil {
		t.Fatal(err)
	}
	payload := db.JobPayload{JobID: jobID, Name: "config", StoredPath: "payloads/hash", SizeBytes: 7, SHA256: strings.Repeat("a", 64), R2Key: "assets/hash"}
	if err := db.InsertJobPayload(database, payload); err != nil {
		t.Fatal(err)
	}
	agentJob := cloud.AgentJob{ID: jobID}
	if err := attachAgentJobPayloads(database, jobID, &agentJob); err != nil {
		t.Fatal(err)
	}
	if len(agentJob.Payloads) != 1 || agentJob.Payloads[0].Name != payload.Name || agentJob.Payloads[0].SHA256 != payload.SHA256 || agentJob.Payloads[0].SizeBytes != payload.SizeBytes {
		t.Fatalf("agent payloads = %+v", agentJob.Payloads)
	}
}
