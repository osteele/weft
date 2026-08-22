package localmutate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
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

func TestConcurrentDaemonAndDirectSubmissions(t *testing.T) {
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

	const n = 20
	errs := make(chan error, 2*n)
	var wg sync.WaitGroup
	for i := range n {
		i := i
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := RecordQueuedJob(context.Background(), database, ops.QueueJobParams{
				WorkingDir:  "/tmp/project",
				Command:     fmt.Sprintf("echo daemon %d", i),
				Description: "daemon stress",
				SubmitToken: fmt.Sprintf("daemon-stress-%d", i),
			})
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := ops.RecordQueuedJob(database, ops.QueueJobParams{
				WorkingDir:  "/tmp/project",
				Command:     fmt.Sprintf("echo direct %d", i),
				Description: "direct stress",
				SubmitToken: fmt.Sprintf("direct-stress-%d", i),
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent submission failed: %v", err)
		}
	}

	var jobs int
	if err := database.QueryRow(`SELECT COUNT(*) FROM jobs WHERE tombstoned = 0`).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 2*n {
		t.Fatalf("job count = %d, want %d", jobs, 2*n)
	}
}

func TestRecordQueuedJobFallsBackWhenDaemonMutationIsSlow(t *testing.T) {
	database := db.SetupTestDB(t)
	withTestDaemon(t, "/tmp/weft-slow-submit.sock")

	oldDialMutation := dialMutationFunc
	dialMutationFunc = func(ctx context.Context, socketPath string, op string, payload any, result any) error {
		<-ctx.Done()
		return ctx.Err()
	}
	t.Cleanup(func() {
		dialMutationFunc = oldDialMutation
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	jobID, err := RecordQueuedJob(ctx, database, ops.QueueJobParams{
		WorkingDir:  "/tmp/project",
		Command:     "echo fallback after slow socket",
		Description: "fallback after slow socket",
		SubmitToken: "slow-submit-token",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.Command != "echo fallback after slow socket" {
		t.Fatalf("job = %+v", job)
	}
	gotID, ok, err := db.FindJobIDBySubmitToken(database, "slow-submit-token")
	if err != nil {
		t.Fatalf("FindJobIDBySubmitToken: %v", err)
	}
	if !ok || gotID != jobID {
		t.Fatalf("submit token lookup = %d,%v; want %d,true", gotID, ok, jobID)
	}
}

func TestRecordQueuedJobSlowDaemonCommitAfterFallbackFailureReturnsExistingJob(t *testing.T) {
	database := db.SetupTestDB(t)
	withTestDaemon(t, "/tmp/weft-submit-race.sock")
	const token = "commit-after-timeout-token"

	oldDialMutation := dialMutationFunc
	dialMutationFunc = func(ctx context.Context, socketPath string, op string, payload any, result any) error {
		return context.DeadlineExceeded
	}
	t.Cleanup(func() {
		dialMutationFunc = oldDialMutation
	})

	oldDirect := recordQueuedJobDirectFunc
	recordQueuedJobDirectFunc = func(database *sql.DB, params ops.QueueJobParams) (int64, error) {
		jobID, err := ops.RecordQueuedJob(database, ops.QueueJobParams{
			WorkingDir:  params.WorkingDir,
			Command:     "echo committed by daemon",
			Description: "committed by daemon",
			SubmitToken: token,
		})
		if err != nil {
			return 0, err
		}
		if jobID <= 0 {
			t.Fatalf("daemon-side insert returned invalid job id %d", jobID)
		}
		return 0, fmt.Errorf("UNIQUE constraint failed: jobs.submit_token")
	}
	t.Cleanup(func() {
		recordQueuedJobDirectFunc = oldDirect
	})

	gotID, err := RecordQueuedJob(context.Background(), database, ops.QueueJobParams{
		WorkingDir:  "/tmp/project",
		Command:     "echo direct fallback",
		Description: "direct fallback",
		SubmitToken: token,
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob: %v", err)
	}
	wantID, ok, err := db.FindJobIDBySubmitToken(database, token)
	if err != nil {
		t.Fatalf("FindJobIDBySubmitToken: %v", err)
	}
	if !ok || gotID != wantID {
		t.Fatalf("RecordQueuedJob returned %d, token lookup = %d,%v", gotID, wantID, ok)
	}
	job, err := db.GetJobByID(database, gotID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil || job.Command != "echo committed by daemon" {
		t.Fatalf("job = %+v, want daemon-committed row", job)
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

// TestRecordQueuedJobRejectsDaemonSuccessWithoutJobID pins that a daemon reply
// of "no error, no job id" is not treated as a submission. Returning (0, nil)
// would tell the caller its job was queued when no row exists, which is
// indistinguishable from success at the exit code and leaves no id to check.
func TestRecordQueuedJobRejectsDaemonSuccessWithoutJobID(t *testing.T) {
	database := db.SetupTestDB(t)
	withTestDaemon(t, "/tmp/weft-submit-zero-id.sock")

	oldDialMutation := dialMutationFunc
	// Succeeds without populating the result: the shape a version skew or a
	// truncated reply produces.
	dialMutationFunc = func(ctx context.Context, socketPath string, op string, payload any, result any) error {
		return nil
	}
	t.Cleanup(func() { dialMutationFunc = oldDialMutation })

	directCalled := false
	oldDirect := recordQueuedJobDirectFunc
	recordQueuedJobDirectFunc = func(database *sql.DB, params ops.QueueJobParams) (int64, error) {
		directCalled = true
		return ops.RecordQueuedJob(database, params)
	}
	t.Cleanup(func() { recordQueuedJobDirectFunc = oldDirect })

	jobID, err := RecordQueuedJob(context.Background(), database, ops.QueueJobParams{
		WorkingDir:  "/tmp/project",
		Command:     "echo zero id",
		Description: "zero id",
		SubmitToken: "zero-id-token",
	})
	if err != nil {
		t.Fatalf("RecordQueuedJob: %v", err)
	}
	if jobID == 0 {
		t.Fatal("returned job id 0 with no error: a submission that did not happen reported as success")
	}
	if !directCalled {
		t.Error("expected the direct fallback to run after the daemon returned no job id")
	}
	if _, err := db.GetJobByID(database, jobID); err != nil {
		t.Errorf("returned job id %d has no row: %v", jobID, err)
	}
}
