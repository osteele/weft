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

// A queue entry rebuilt outside admission (describe edit, deferred update,
// requeue) must re-stage the payloads admission accepted; dropping them
// stranded wj7685 with an unset WEFT_PAYLOAD_DIR (wb126).
func TestQueueEntryForJobRebuildsStagedInputs(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp/project", "agent-execution-worker execute --prompt-payload execution-prompt", "", "")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("ab", 32)
	if err := db.InsertJobPayload(database, db.JobPayload{
		JobID: jobID, Name: "execution-prompt", StoredPath: "payloads/" + digest,
		SizeBytes: 7, SHA256: digest, R2Key: "assets/" + digest,
	}); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}

	entry, err := queueEntryForJob(database, job, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entry.Payloads) != 1 || entry.Payloads[0].Name != "execution-prompt" || entry.Payloads[0].SHA256 != digest {
		t.Fatalf("rebuilt payloads = %+v", entry.Payloads)
	}
	if !strings.Contains(entry.Command, "WEFT_PAYLOAD_DIR") || !strings.HasSuffix(entry.Command, job.Command) {
		t.Fatalf("rebuilt command = %q", entry.Command)
	}
}

func TestApplyQueueUpdatePreservesPayloads(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp/project", "agent-execution-worker execute --prompt-payload execution-prompt", "", "")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("ab", 32)
	if err := db.InsertJobPayload(database, db.JobPayload{
		JobID: jobID, Name: "execution-prompt", StoredPath: "payloads/" + digest,
		SizeBytes: 7, SHA256: digest, R2Key: "assets/" + digest,
	}); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}

	var written string
	mockSSHFunc(t, func(_, command string) (string, string, int) {
		written = command
		return "", "", 0
	})
	if err := applyQueueUpdate(database, job, job.EnvVars, job.DepSpec, time.Second); err != nil {
		t.Fatal(err)
	}
	// The ssh command carries the job JSON through Go %q quoting, so match
	// on payload identity rather than exact quote placement.
	if !strings.Contains(written, "payloads") || !strings.Contains(written, "execution-prompt") || !strings.Contains(written, "WEFT_PAYLOAD_DIR") {
		t.Fatalf("queue job file write = %q", written)
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
	mockSSHFunc(t, func(_, command string) (string, string, int) {
		if strings.Contains(command, "__WEFT_NO_STATE_FILE__") {
			return `{"agent_version":"test-agent","queue_protocol_version":1,"capabilities":["job-payload-v1"],"pending":[]}` + "\n", "", 0
		}
		return "", "", 0
	})
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

func TestAppendJobToQueueRejectsLegacyRunnerProtocol(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "host-alpha", "/tmp/project", "true", "", "")
	if err != nil {
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
	appended := false
	appendQueueCommandSSH = func(_ string, _ opsqueue.QueueCommand, _ opsqueue.AppendCommandOptions) error {
		appended = true
		return nil
	}
	mockSSHFunc(t, func(_, command string) (string, string, int) {
		if strings.Contains(command, "__WEFT_NO_STATE_FILE__") {
			return `{"pending":[]}` + "\n", "", 0
		}
		return "", "", 0
	})

	err = AppendJobToQueue(database, job, time.Second)
	if err == nil || !strings.Contains(err.Error(), "older than required") ||
		!strings.Contains(err.Error(), "weft queue update host-alpha") {
		t.Fatalf("error = %v, want actionable protocol incompatibility", err)
	}
	if appended {
		t.Fatal("job was appended to a legacy runner")
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
			return `{"agent_version":"test-agent","queue_protocol_version":1,"capabilities":` + capabilities + `,"pending":[]}` + "\n", "", 0
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
