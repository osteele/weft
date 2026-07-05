package localmutate

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/osteele/weft/internal/daemonapi"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
)

func TestSetProcessedTagRoutesThroughDaemonMutation(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := ops.RecordQueuedJob(database, ops.QueueJobParams{
		WorkingDir: "/tmp/project",
		Command:    "echo hi",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob: %v", err)
	}
	socketPath := testDaemonSocketPath(t)
	server, err := daemonapi.StartServerWithOptions(t.Context(), database, socketPath, daemonapi.ServerOptions{
		Mutate: Handler,
	})
	if err != nil {
		t.Fatalf("StartServerWithOptions: %v", err)
	}
	defer server.Close()
	withTestDaemon(t, socketPath)

	if err := SetProcessedTag(context.Background(), database, jobID, true); err != nil {
		t.Fatalf("SetProcessedTag: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || !job.HasTag(db.ProcessedTag) {
		t.Fatalf("processed tag missing from job %+v", job)
	}
}

func TestSetProcessedTagFallsBackWithoutDaemon(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := ops.RecordQueuedJob(database, ops.QueueJobParams{
		WorkingDir: "/tmp/project",
		Command:    "echo hi",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob: %v", err)
	}
	withNoDaemon(t)

	if err := SetProcessedTag(context.Background(), database, jobID, true); err != nil {
		t.Fatalf("SetProcessedTag: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || !job.HasTag(db.ProcessedTag) {
		t.Fatalf("processed tag missing from job %+v", job)
	}
}

func TestRecordQueuedJobUsesMutationBeforeLegacySubmit(t *testing.T) {
	database := db.SetupTestDB(t)
	socketPath := testDaemonSocketPath(t)
	server, err := daemonapi.StartServerWithOptions(t.Context(), database, socketPath, daemonapi.ServerOptions{
		Mutate: Handler,
	})
	if err != nil {
		t.Fatalf("StartServerWithOptions: %v", err)
	}
	defer server.Close()
	withTestDaemon(t, socketPath)

	jobID, err := RecordQueuedJob(context.Background(), database, ops.QueueJobParams{
		WorkingDir:  "/tmp/project",
		Command:     "echo daemon",
		Description: "daemon mutation",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.Command != "echo daemon" || job.Description != "daemon mutation" {
		t.Fatalf("job = %+v", job)
	}
}

func TestRecordQueuedJobFallsBackWithoutDaemon(t *testing.T) {
	database := db.SetupTestDB(t)
	withNoDaemon(t)

	jobID, err := RecordQueuedJob(context.Background(), database, ops.QueueJobParams{
		WorkingDir:  "/tmp/project",
		Command:     "echo fallback",
		Description: "fallback",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.Command != "echo fallback" {
		t.Fatalf("job = %+v", job)
	}
}

func testDaemonSocketPath(t *testing.T) string {
	t.Helper()
	path := fmt.Sprintf("/tmp/weft-localmutate-%d-%d.sock", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func withTestDaemon(t *testing.T, socketPath string) {
	t.Helper()
	oldPaths := daemonPathsFunc
	oldStatus := daemonStatusFunc
	daemonPathsFunc = func() daemoncontrol.Paths {
		return daemoncontrol.Paths{SocketFile: socketPath}
	}
	daemonStatusFunc = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
		return daemoncontrol.Status{Live: true}, nil
	}
	t.Cleanup(func() {
		daemonPathsFunc = oldPaths
		daemonStatusFunc = oldStatus
	})
}

func withNoDaemon(t *testing.T) {
	t.Helper()
	oldStatus := daemonStatusFunc
	daemonStatusFunc = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
		return daemoncontrol.Status{Live: false}, nil
	}
	t.Cleanup(func() {
		daemonStatusFunc = oldStatus
	})
}
