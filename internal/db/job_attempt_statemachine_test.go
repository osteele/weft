package db

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/osteele/weft/internal/status"
)

// TestJobAttemptStateMachine_RandomCommands exercises the persisted job-attempt
// lifecycle with randomized command sequences. It checks:
//   - high-level sync commands (RecordCompletionByID, MarkDeadByID) only accept
//     valid transitions
//   - terminal attempts always have end_time
//   - at most one open authoritative attempt exists at a time
//   - the job_status view reflects the authoritative attempt
//   - last_synced_status is updated only by sync operations
//
// The test logs the random seed on failure; set WEFT_TEST_SEED to reproduce.
func TestJobAttemptStateMachine_RandomCommands(t *testing.T) {
	seed := jobAttemptSMSeed()
	rng := rand.New(rand.NewPCG(seed, 0))
	t.Logf("job-attempt state-machine seed: %d (set WEFT_TEST_SEED to reproduce)", seed)

	iterations := 50
	stepsPerRun := 30
	if testing.Short() {
		iterations = 10
		stepsPerRun = 15
	}

	for i := 0; i < iterations; i++ {
		database := SetupTestDB(t)

		jobID, err := RecordQueued(database, "host", "/tmp/project", "echo test", "sm-test")
		if err != nil {
			t.Fatalf("iteration %d: RecordQueued: %v", i, err)
		}

		m := &jobModel{
			status:     StatusQueued,
			hasOpen:    true,
			lastSynced: "",
		}

		for step := 0; step < stepsPerRun; step++ {
			cmd := randomJobCommand(rng, m)
			desc := cmd.describe()

			err := cmd.run(database, jobID)
			cmd.apply(m, err)

			checkJobAttemptInvariants(t, database, jobID, m, i, step, desc, seed)
		}
	}
}

// jobModel is the reference state for one job. It intentionally does not model
// every column; it tracks just enough to verify transitions and the
// last_synced_status side effect.
type jobModel struct {
	status     string
	hasOpen    bool
	lastSynced string
}

// jobCommand is one mutating operation against a job.
type jobCommand interface {
	describe() string
	run(db *sql.DB, jobID int64) error
	apply(m *jobModel, err error)
}

func randomJobCommand(rng *rand.Rand, m *jobModel) jobCommand {
	// Bias toward commands that are likely to change state, but keep some
	// invalid-transition commands so we exercise rejection paths.
	switch rng.IntN(8) {
	case 0:
		return recordCompletionCmd{}
	case 1:
		return markDeadCmd{}
	case 2:
		return markQueuedCmd{}
	case 3:
		return markRunningCmd{}
	case 4:
		return markStartingCmd{}
	case 5:
		return resetUnplacedCmd{}
	case 6:
		status, exitCode := randomTerminalStatus(rng)
		return closeAttemptCmd{status: status, exitCode: exitCode}
	default:
		return createAttemptCmd{status: randomAttemptStatus(rng)}
	}
}

type recordCompletionCmd struct{}

func (c recordCompletionCmd) describe() string { return "RecordCompletionByID(0)" }
func (c recordCompletionCmd) run(db *sql.DB, jobID int64) error {
	now := time.Now().Unix()
	return RecordCompletionByID(db, jobID, 0, now)
}
func (c recordCompletionCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	// RecordCompletionByID returns nil even when there is no attempt or the
	// transition is invalid, because the underlying UPDATE matches zero rows.
	// Only mutate the model when the transition is actually allowed.
	if _, transitionErr := status.ValidateTransition(m.status, StatusCompleted, true); transitionErr != nil {
		return
	}
	m.status = StatusCompleted
	m.lastSynced = StatusCompleted
	m.hasOpen = false
}

type markDeadCmd struct{}

func (c markDeadCmd) describe() string { return "MarkDeadByID" }
func (c markDeadCmd) run(db *sql.DB, jobID int64) error {
	return MarkDeadByID(db, jobID)
}
func (c markDeadCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	// MarkDeadByID uses checkOpenTransition, which returns nil when there is no
	// open attempt. Guard against mutating the model when the DB had nothing to
	// update.
	if !m.hasOpen {
		return
	}
	if _, transitionErr := status.ValidateTransition(m.status, StatusFailed, false); transitionErr != nil {
		return
	}
	m.status = StatusFailed
	m.lastSynced = StatusFailed
	m.hasOpen = false
}

type markQueuedCmd struct{}

func (c markQueuedCmd) describe() string { return "MarkQueuedByID" }
func (c markQueuedCmd) run(db *sql.DB, jobID int64) error {
	return MarkQueuedByID(db, jobID)
}
func (c markQueuedCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	m.status = StatusQueued
	m.lastSynced = StatusQueued
	m.hasOpen = true
}

type markRunningCmd struct{}

func (c markRunningCmd) describe() string { return "MarkQueuedJobRunning" }
func (c markRunningCmd) run(db *sql.DB, jobID int64) error {
	return MarkQueuedJobRunning(db, jobID)
}
func (c markRunningCmd) apply(m *jobModel, err error) {
	if err != nil || !m.hasOpen {
		return
	}
	m.status = StatusRunning
	m.lastSynced = StatusRunning
}

type markStartingCmd struct{}

func (c markStartingCmd) describe() string { return "MarkQueuedJobStarting" }
func (c markStartingCmd) run(db *sql.DB, jobID int64) error {
	return MarkQueuedJobStarting(db, jobID)
}
func (c markStartingCmd) apply(m *jobModel, err error) {
	if err != nil || !m.hasOpen {
		return
	}
	m.status = StatusStarting
	m.lastSynced = StatusStarting
}

type resetUnplacedCmd struct{}

func (c resetUnplacedCmd) describe() string { return "ResetJobToUnplaced" }
func (c resetUnplacedCmd) run(db *sql.DB, jobID int64) error {
	return ResetJobToUnplaced(db, jobID)
}
func (c resetUnplacedCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	m.status = StatusQueued
	m.lastSynced = ""
	m.hasOpen = true
}

type closeAttemptCmd struct {
	status   string
	exitCode *int
}

func (c closeAttemptCmd) describe() string {
	return fmt.Sprintf("CloseAttempt(%s, %v)", c.status, c.exitCode)
}
func (c closeAttemptCmd) run(db *sql.DB, jobID int64) error {
	return CloseAttempt(db, jobID, c.status, c.exitCode, time.Now().Unix())
}
func (c closeAttemptCmd) apply(m *jobModel, err error) {
	if err != nil || !m.hasOpen {
		return
	}
	m.status = c.status
	m.hasOpen = false
}

type createAttemptCmd struct {
	status string
}

func (c createAttemptCmd) describe() string {
	return fmt.Sprintf("CreateAttempt(%s)", c.status)
}
func (c createAttemptCmd) run(db *sql.DB, jobID int64) error {
	_, err := CreateAttempt(db, jobID, "host", nil, c.status)
	return err
}
func (c createAttemptCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	m.status = c.status
	m.lastSynced = ""
	m.hasOpen = !IsTerminalStatus(c.status)
}

func randomTerminalStatus(rng *rand.Rand) (string, *int) {
	statuses := []string{StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled}
	status := statuses[rng.IntN(len(statuses))]
	var exitCode *int
	switch status {
	case StatusCompleted:
		code := 0
		exitCode = &code
	case StatusFailed:
		code := rng.IntN(5) + 1
		exitCode = &code
	}
	return status, exitCode
}

func randomAttemptStatus(rng *rand.Rand) string {
	statuses := []string{
		StatusQueued, StatusStarting, StatusRunning, StatusPaused,
		StatusCompleted, StatusFailed, StatusDead, StatusKilled, StatusCanceled,
	}
	return statuses[rng.IntN(len(statuses))]
}

// checkJobAttemptInvariants verifies that the persisted state matches the model
// and the spec invariants.
func checkJobAttemptInvariants(t *testing.T, db *sql.DB, jobID int64, m *jobModel, iteration, step int, desc string, seed uint64) {
	t.Helper()

	job, err := GetJobByID(db, jobID)
	if err != nil {
		t.Fatalf("iteration %d step %d: GetJobByID: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	if job == nil {
		t.Fatalf("iteration %d step %d: job %d missing\nseed=%d command=%s", iteration, step, jobID, seed, desc)
	}

	if job.Status != m.status {
		t.Fatalf("iteration %d step %d: job status = %q, want %q\nseed=%d command=%s",
			iteration, step, job.Status, m.status, seed, desc)
	}
	if job.LastSyncedStatus != m.lastSynced {
		t.Fatalf("iteration %d step %d: last_synced_status = %q, want %q\nseed=%d command=%s",
			iteration, step, job.LastSyncedStatus, m.lastSynced, seed, desc)
	}

	attempts, err := ListAttempts(db, jobID)
	if err != nil {
		t.Fatalf("iteration %d step %d: ListAttempts: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	t.Logf("iteration %d step %d: command=%s model=%+v attempts=%+v job=%+v", iteration, step, desc, m, attempts, job)

	var openCount int
	for i, a := range attempts {
		if a.EndTime == nil {
			openCount++
		}
		if IsTerminalStatus(a.Status) && a.EndTime == nil {
			t.Fatalf("iteration %d step %d: terminal attempt %d status %q has no end_time\nseed=%d command=%s",
				iteration, step, a.ID, a.Status, seed, desc)
		}
		if a.EndTime != nil && *a.EndTime <= 0 {
			t.Fatalf("iteration %d step %d: attempt %d has non-positive end_time %d\nseed=%d command=%s",
				iteration, step, a.ID, *a.EndTime, seed, desc)
		}
		if i > 0 && a.AttemptNumber >= attempts[i-1].AttemptNumber {
			t.Fatalf("iteration %d step %d: attempt numbers not strictly decreasing: %v\nseed=%d command=%s",
				iteration, step, attempts, seed, desc)
		}
	}
	if openCount > 1 {
		t.Fatalf("iteration %d step %d: %d open attempts\nseed=%d command=%s",
			iteration, step, openCount, seed, desc)
	}

	authID, err := GetAuthoritativeAttemptID(db, jobID)
	if err != nil {
		t.Fatalf("iteration %d step %d: GetAuthoritativeAttemptID: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	if len(attempts) == 0 {
		if authID != 0 {
			t.Fatalf("iteration %d step %d: no attempts but authoritative id = %d\nseed=%d command=%s",
				iteration, step, authID, seed, desc)
		}
	} else {
		if authID != attempts[0].ID {
			t.Fatalf("iteration %d step %d: authoritative attempt = %d, want %d\nseed=%d command=%s",
				iteration, step, authID, attempts[0].ID, seed, desc)
		}
		if job.LatestRunID == nil || *job.LatestRunID != authID {
			t.Fatalf("iteration %d step %d: latest_run_id = %v, want %d\nseed=%d command=%s",
				iteration, step, job.LatestRunID, authID, seed, desc)
		}
	}

	// Verify transition-checking commands behaved according to the spec table.
	// (We only need to check the cases where we predicted an error.)
	if desc == "RecordCompletionByID(0)" {
		if _, transitionErr := status.ValidateTransition(m.status, StatusCompleted, true); transitionErr != nil {
			// If the transition is invalid, the previous apply should not have
			// changed the model; the invariant checks above already confirm the
			// model matches the DB, which means the DB also rejected it.
		}
	}
}

func jobAttemptSMSeed() uint64 {
	if s := os.Getenv("WEFT_TEST_SEED"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			return n
		}
	}
	return rand.Uint64()
}
