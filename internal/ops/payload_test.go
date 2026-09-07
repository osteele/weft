package ops

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/opsqueue"
)

func TestQueuePayloadsForJobPreservesIdentity(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp/project", "true", "", "")
	if err != nil {
		t.Fatal(err)
	}
	payload := db.JobPayload{JobID: jobID, Name: "config", StoredPath: "payloads/hash", SizeBytes: 7, SHA256: strings.Repeat("a", 64), R2Key: "assets/hash"}
	if err := db.InsertJobPayload(database, payload); err != nil {
		t.Fatal(err)
	}
	got, err := queuePayloadsForJob(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != payload.Name || got[0].SHA256 != payload.SHA256 || got[0].SizeBytes != payload.SizeBytes || got[0].R2Key != payload.R2Key {
		t.Fatalf("queue payloads = %+v", got)
	}
}

func TestPayloadGuardedCommandFailsClosedOnOldAgent(t *testing.T) {
	command := payloadGuardedCommand("python train.py", []opsqueue.Payload{{Name: "config"}})
	if !strings.Contains(command, `WEFT_PAYLOAD_DIR`) || !strings.Contains(command, "exit 78") || !strings.HasSuffix(command, "python train.py") {
		t.Fatalf("guarded command = %q", command)
	}
	if got := payloadGuardedCommand("python train.py", nil); got != "python train.py" {
		t.Fatalf("unguarded command = %q", got)
	}
}

func TestAppendJobToQueueWiresPayloadGuard(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp/project", "python train.py", "", "")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	if err := db.InsertJobPayload(database, db.JobPayload{
		JobID: jobID, Name: "config", StoredPath: "payloads/" + digest,
		SizeBytes: 7, SHA256: digest, R2Key: "assets/" + digest,
	}); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	originalLoad := loadQueueConfig
	originalAppend := appendQueueCommandSSH
	t.Cleanup(func() {
		loadQueueConfig = originalLoad
		appendQueueCommandSSH = originalAppend
	})
	loadQueueConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	var sent opsqueue.QueueCommand
	appendQueueCommandSSH = func(_ string, command opsqueue.QueueCommand, _ opsqueue.AppendCommandOptions) error {
		sent = command
		return nil
	}
	if err := AppendJobToQueue(database, job, time.Second); err != nil {
		t.Fatal(err)
	}
	if sent.Job == nil || !strings.Contains(sent.Job.Cmd, "WEFT_PAYLOAD_DIR") || !strings.HasSuffix(sent.Job.Cmd, job.Command) {
		t.Fatalf("queued command = %#v", sent.Job)
	}
}

func TestAppendIsolatedJobResolvesNamedAssetForAgentStaging(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp/project", "python train.py", "", "")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("b", 64)
	if err := db.UpsertNamedAsset(database, db.NamedAsset{
		Name:        "trace",
		ContentHash: digest,
		ContentType: "file",
		TargetPath:  "data/trace.jsonl",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobNeeds(database, jobID, []string{"asset:trace"}); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	originalLoad := loadQueueConfig
	originalAppend := appendQueueCommandSSH
	t.Cleanup(func() {
		loadQueueConfig = originalLoad
		appendQueueCommandSSH = originalAppend
	})
	loadQueueConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	var sent opsqueue.QueueCommand
	appendQueueCommandSSH = func(_ string, command opsqueue.QueueCommand, _ opsqueue.AppendCommandOptions) error {
		sent = command
		return nil
	}
	supportsArtifactNeeds := false
	mockSSHFunc(t, func(_, command string) (string, string, int) {
		if strings.Contains(command, "__WEFT_NO_STATE_FILE__") {
			capabilities := "[]"
			if supportsArtifactNeeds {
				capabilities = `["artifact-need-v1"]`
			}
			return `{"capabilities":` + capabilities + `,"pending":[]}` + "\n", "", 0
		}
		return "", "", 0
	})

	err = AppendJobToQueueWithSourceAndR2(database, job, time.Second, digest, "sources/snapshot.tar.gz")
	if err == nil || !opsqueue.IsMissingRunnerCapabilityBlock(err.Error()) {
		t.Fatalf("incompatible runner error = %v", err)
	}
	if sent.Job != nil {
		t.Fatal("job was appended before runner capability was confirmed")
	}
	supportsArtifactNeeds = true

	if err := AppendJobToQueueWithSourceAndR2(database, job, time.Second, digest, "sources/snapshot.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if sent.Job == nil || len(sent.Job.ArtifactNeeds) != 1 {
		t.Fatalf("queued artifact needs = %#v", sent.Job)
	}
	need := sent.Job.ArtifactNeeds[0]
	if need.Spec != "asset:trace" || need.Path != "data/trace.jsonl" || need.R2Key != "assets/"+digest {
		t.Fatalf("queued artifact need = %#v", need)
	}
	if !strings.Contains(sent.Job.Cmd, "WEFT_ARTIFACT_NEEDS_STAGED") || !strings.HasSuffix(sent.Job.Cmd, job.Command) {
		t.Fatalf("queued command = %q", sent.Job.Cmd)
	}
}
