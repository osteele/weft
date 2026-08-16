package db

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/osteele/weft/internal/status"
)

// TestJobAttemptStateMachine_RandomCommands exercises the persisted job-attempt
// lifecycle with randomized command sequences. It checks:
//   - high-level sync commands (RecordCompletionByID, MarkDeadByID) only accept
//     valid transitions
//   - cloud completion (RecordCloudJobCompletion) follows the same authoritative
//     rules as SSH sync completion
//   - terminal attempts always have end_time
//   - at most one open authoritative attempt exists at a time
//   - abandoned attempts are removed from authority and the next eligible attempt
//     becomes authoritative
//   - move intents hide the target attempt until confirmed; confirmation abandons
//     the source and promotes the target
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
	for i := 0; i < iterations; i++ {
		database := SetupTestDB(t)

		jobID, err := RecordQueued(database, "host", "/tmp/project", "echo test", "sm-test")
		if err != nil {
			t.Fatalf("iteration %d: RecordQueued: %v", i, err)
		}

		m := &jobModel{
			attempts: []attemptModel{{status: StatusQueued}},
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

// attemptModel is one logical job attempt in the reference model.
type attemptModel struct {
	status     string
	closed     bool // end_time IS NOT NULL
	abandoned  bool
	hidden     bool // bound to an open move intent
	lastSynced string
}

// effectiveStatus returns the status the job_status view would derive from this
// attempt. A closed non-terminal attempt is reported as dead by the view.
func (a attemptModel) effectiveStatus() string {
	if a.closed && !IsTerminalStatus(a.status) {
		return StatusDead
	}
	return a.status
}

// intentModel tracks an open or resolved move intent.
type intentModel struct {
	state     string
	sourceIdx int
	targetIdx int // -1 until a target attempt is created
}

// jobModel is the reference state for one job. Attempts are stored newest first
// to match DB ordering by attempt_number DESC.
type jobModel struct {
	attempts        []attemptModel
	intent          *intentModel
	requestedStatus string // "" means NULL in the jobs table
}

// authIdx returns the newest non-abandoned, non-hidden attempt index, or -1.
func (m *jobModel) authIdx() int {
	for i, a := range m.attempts {
		if !a.abandoned && !a.hidden {
			return i
		}
	}
	return -1
}

// openAuthIdx returns the newest open authoritative attempt index, or -1.
func (m *jobModel) openAuthIdx() int {
	for i, a := range m.attempts {
		if !a.closed && !a.abandoned && !a.hidden {
			return i
		}
	}
	return -1
}

// openIdx returns the newest open, non-hidden attempt index, regardless of
// abandoned state. Several DB helpers use latestOpenAttemptSubquery, which
// selects from job_open_attempts without filtering abandoned_at.
func (m *jobModel) openIdx() int {
	for i, a := range m.attempts {
		if !a.closed && !a.hidden {
			return i
		}
	}
	return -1
}

// newestIdx returns the newest non-abandoned attempt index, or -1.
func (m *jobModel) newestIdx() int {
	for i := range m.attempts {
		if !m.attempts[i].abandoned {
			return i
		}
	}
	return -1
}

// expectedStatus derives the job status from the authoritative attempt or the
// requested_status fallback, mirroring the job_status view.
func (m *jobModel) expectedStatus() string {
	if idx := m.authIdx(); idx >= 0 {
		return m.attempts[idx].effectiveStatus()
	}
	if m.requestedStatus != "" {
		return m.requestedStatus
	}
	return StatusDraft
}

// expectedLastSynced returns the last_synced_status from the authoritative
// attempt, or empty when there is none.
func (m *jobModel) expectedLastSynced() string {
	if idx := m.authIdx(); idx >= 0 {
		return m.attempts[idx].lastSynced
	}
	return ""
}

// closeOpen marks every open attempt closed without changing its status. This
// mirrors closeOpenAttempts, which only stamps end_time.
func (m *jobModel) closeOpen() {
	for i := range m.attempts {
		if !m.attempts[i].closed {
			m.attempts[i].closed = true
		}
	}
}

// closeOpenCanceled marks every open attempt closed and sets its status to
// canceled. This mirrors closeAttemptsAndRequeueWithOutcome when no outcome is
// supplied.
func (m *jobModel) closeOpenCanceled() {
	for i := range m.attempts {
		if !m.attempts[i].closed {
			m.attempts[i].closed = true
			m.attempts[i].status = StatusCanceled
		}
	}
}

// prependAttempt adds a new attempt at the front and shifts intent indices.
func (m *jobModel) prependAttempt(a attemptModel) {
	m.attempts = append([]attemptModel{a}, m.attempts...)
	if m.intent != nil {
		m.intent.sourceIdx++
		if m.intent.targetIdx >= 0 {
			m.intent.targetIdx++
		}
	}
}

// maybeAutoConfirmTarget mirrors the database trigger
// move_intents_auto_confirm_on_target_attempt_end: when the hidden target of an
// open move intent becomes terminal (completed/failed) with an end_time, the
// source attempt is abandoned and the intent is confirmed so the target becomes
// authoritative.
func (m *jobModel) maybeAutoConfirmTarget(idx int) {
	if m.intent == nil || m.intent.state != string(MoveIntentStateOpen) {
		return
	}
	if m.intent.targetIdx != idx {
		return
	}
	a := m.attempts[idx]
	if !a.closed {
		return
	}
	if a.status != StatusCompleted && a.status != StatusFailed {
		return
	}
	m.attempts[m.intent.sourceIdx].abandoned = true
	m.attempts[idx].hidden = false
	m.intent.state = string(MoveIntentStateConfirmed)
}

// maybeAutoObsoleteSource mirrors the database trigger
// move_intents_auto_obsolete_on_source_attempt_end: when the source attempt of
// an open move intent finishes as completed/failed with an end_time, the hidden
// target is abandoned and the intent is obsoleted.
func (m *jobModel) maybeAutoObsoleteSource(idx int) {
	if m.intent == nil || m.intent.state != string(MoveIntentStateOpen) {
		return
	}
	if m.intent.sourceIdx != idx {
		return
	}
	a := m.attempts[idx]
	if !a.closed {
		return
	}
	if a.status != StatusCompleted && a.status != StatusFailed {
		return
	}
	if m.intent.targetIdx >= 0 {
		m.attempts[m.intent.targetIdx].abandoned = true
		m.attempts[m.intent.targetIdx].hidden = false
	}
	m.intent.state = string(MoveIntentStateObsoleted)
}

// jobCommand is one mutating operation against a job.
type jobCommand interface {
	describe() string
	run(db *sql.DB, jobID int64) error
	apply(m *jobModel, err error)
}

func randomJobCommand(rng *rand.Rand, m *jobModel) jobCommand {
	// Build a list of eligible commands each step so move-intent and abandon
	// commands are only emitted in states where the model expects them to
	// succeed.
	var eligible []jobCommand

	eligible = append(eligible, recordCompletionCmd{})
	eligible = append(eligible, markDeadCmd{})
	eligible = append(eligible, markQueuedCmd{})
	eligible = append(eligible, markRunningCmd{})
	eligible = append(eligible, markStartingCmd{})
	eligible = append(eligible, resetUnplacedCmd{})
	status, exitCode := randomTerminalStatus(rng)
	eligible = append(eligible, closeAttemptCmd{status: status, exitCode: exitCode})
	eligible = append(eligible, createAttemptCmd{status: randomAttemptStatus(rng)})
	eligible = append(eligible, recordCloudCompletionCmd{exitCode: rng.IntN(2)})

	if m.newestIdx() >= 0 {
		eligible = append(eligible, abandonAttemptCmd{})
	}
	if m.intent == nil && m.openAuthIdx() >= 0 {
		eligible = append(eligible, createMoveIntentCmd{})
	}
	if m.intent != nil && m.intent.state == string(MoveIntentStateOpen) && m.intent.targetIdx < 0 {
		eligible = append(eligible, createMoveTargetAttemptCmd{status: randomMoveTargetStatus(rng)})
	}
	if m.intent != nil && m.intent.state == string(MoveIntentStateOpen) && m.intent.targetIdx >= 0 {
		eligible = append(eligible, confirmMoveTargetCmd{})
	}

	return eligible[rng.IntN(len(eligible))]
}

type recordCompletionCmd struct{}

func (c recordCompletionCmd) describe() string { return "RecordCompletionByID(0)" }
func (c recordCompletionCmd) run(db *sql.DB, jobID int64) error {
	now := time.Now().Unix()
	return RecordCompletionByID(db, jobID, 0, now)
}
func (c recordCompletionCmd) apply(m *jobModel, err error) {
	if err != nil || len(m.attempts) == 0 {
		return
	}
	// RecordCompletionByID targets the physical latest attempt. The transition
	// guard uses that attempt's status, not the authoritative job status.
	if _, transitionErr := status.ValidateTransition(m.attempts[0].status, StatusCompleted, true); transitionErr != nil {
		return
	}
	m.attempts[0].status = StatusCompleted
	m.attempts[0].closed = true
	m.attempts[0].lastSynced = StatusCompleted
	m.maybeAutoConfirmTarget(0)
	m.maybeAutoObsoleteSource(0)
}

type recordCloudCompletionCmd struct {
	exitCode int
}

func (c recordCloudCompletionCmd) describe() string {
	return fmt.Sprintf("RecordCloudJobCompletion(%d)", c.exitCode)
}
func (c recordCloudCompletionCmd) run(db *sql.DB, jobID int64) error {
	now := time.Now().Unix()
	_, err := RecordCloudJobCompletion(db, jobID, c.exitCode, now-10, now, "", "", time.Time{}, 0)
	return err
}
func (c recordCloudCompletionCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	target := StatusCompleted
	if c.exitCode != 0 {
		target = StatusFailed
	}
	idx := m.authIdx()
	if idx < 0 {
		return
	}
	// RecordCloudJobCompletion's checkTransition reads the physical latest
	// attempt, but the write targets the authoritative attempt.
	if _, transitionErr := status.ValidateTransition(m.attempts[0].status, target, true); transitionErr != nil {
		return
	}
	m.attempts[idx].status = target
	m.attempts[idx].closed = true
	m.attempts[idx].lastSynced = target
	m.maybeAutoConfirmTarget(idx)
	m.maybeAutoObsoleteSource(idx)
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
	idx := m.openIdx()
	if idx < 0 {
		return
	}
	if _, transitionErr := status.ValidateTransition(m.attempts[idx].status, StatusFailed, false); transitionErr != nil {
		return
	}
	m.attempts[idx].status = StatusFailed
	m.attempts[idx].closed = true
	m.attempts[idx].lastSynced = StatusFailed
	m.maybeAutoObsoleteSource(idx)
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
	// In this test all attempts are on-prem (no launch_id), so MarkQueuedByID
	// always closes open attempts and creates a fresh queued attempt.
	m.closeOpen()
	m.prependAttempt(attemptModel{status: StatusQueued, lastSynced: StatusQueued})
}

type markRunningCmd struct{}

func (c markRunningCmd) describe() string { return "MarkQueuedJobRunning" }
func (c markRunningCmd) run(db *sql.DB, jobID int64) error {
	return MarkQueuedJobRunning(db, jobID)
}
func (c markRunningCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	idx := m.openIdx()
	if idx < 0 {
		return
	}
	m.attempts[idx].status = StatusRunning
	m.attempts[idx].lastSynced = StatusRunning
}

type markStartingCmd struct{}

func (c markStartingCmd) describe() string { return "MarkQueuedJobStarting" }
func (c markStartingCmd) run(db *sql.DB, jobID int64) error {
	return MarkQueuedJobStarting(db, jobID)
}
func (c markStartingCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	idx := m.openIdx()
	if idx < 0 {
		return
	}
	m.attempts[idx].status = StatusStarting
	m.attempts[idx].lastSynced = StatusStarting
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
	m.closeOpenCanceled()
	if m.intent != nil && m.intent.state == string(MoveIntentStateOpen) {
		// closeAttemptsAndRequeue abandons the hidden target and cancels the
		// intent before creating the replacement attempt.
		if m.intent.targetIdx >= 0 {
			m.attempts[m.intent.targetIdx].abandoned = true
			m.attempts[m.intent.targetIdx].hidden = false
		}
		m.intent.state = string(MoveIntentStateCanceled)
	}
	m.prependAttempt(attemptModel{status: StatusQueued})
	m.requestedStatus = StatusQueued
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
	if err != nil {
		return
	}
	idx := m.openIdx()
	if idx < 0 {
		return
	}
	m.attempts[idx].status = c.status
	m.attempts[idx].closed = true
	m.maybeAutoConfirmTarget(idx)
	m.maybeAutoObsoleteSource(idx)
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
	m.closeOpen()
	m.prependAttempt(attemptModel{status: c.status, closed: IsTerminalStatus(c.status)})
}

type abandonAttemptCmd struct{}

func (c abandonAttemptCmd) describe() string { return "AbandonAttempt" }
func (c abandonAttemptCmd) run(db *sql.DB, jobID int64) error {
	attemptID, err := GetAuthoritativeAttemptID(db, jobID)
	if err != nil {
		return err
	}
	if attemptID == 0 {
		attemptID, err = GetLatestAttemptID(db, jobID)
		if err != nil || attemptID == 0 {
			return fmt.Errorf("no attempt to abandon")
		}
	}
	return AbandonAttempt(db, attemptID, "state-machine test", nil)
}
func (c abandonAttemptCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	idx := m.authIdx()
	if idx < 0 {
		idx = m.newestIdx()
	}
	if idx < 0 {
		return
	}
	m.attempts[idx].abandoned = true
	// Abandoning the source attempt of an open move intent obsoletes the
	// intent via trigger move_intents_auto_obsolete_on_source_attempt_abandoned.
	// Unlike terminal-source obsolescence, this trigger does not abandon the
	// target; it simply unhides it so it can become authoritative.
	if m.intent != nil && m.intent.state == string(MoveIntentStateOpen) && m.intent.sourceIdx == idx {
		if m.intent.targetIdx >= 0 {
			m.attempts[m.intent.targetIdx].hidden = false
		}
		m.intent.state = string(MoveIntentStateObsoleted)
	}
}

type createMoveIntentCmd struct{}

func (c createMoveIntentCmd) describe() string { return "CreateMoveIntent" }
func (c createMoveIntentCmd) run(db *sql.DB, jobID int64) error {
	_, err := CreateMoveIntent(db, CreateMoveIntentParams{
		JobID:      jobID,
		TargetKind: MoveTargetExisting,
		TargetHost: "target-host",
	})
	return err
}
func (c createMoveIntentCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	idx := m.openAuthIdx()
	if idx < 0 {
		return
	}
	// The source attempt stays authoritative until the intent is confirmed;
	// only the target attempt is hidden while the intent is open.
	m.intent = &intentModel{
		state:     string(MoveIntentStateOpen),
		sourceIdx: idx,
		targetIdx: -1,
	}
}

type createMoveTargetAttemptCmd struct {
	status string
}

func (c createMoveTargetAttemptCmd) describe() string {
	return fmt.Sprintf("CreateMoveTargetAttempt(%s)", c.status)
}
func (c createMoveTargetAttemptCmd) run(db *sql.DB, jobID int64) error {
	intent, err := GetOpenMoveIntent(db, jobID)
	if err != nil {
		return err
	}
	if intent == nil {
		return fmt.Errorf("no open move intent")
	}
	_, err = CreateMoveTargetAttempt(db, intent.ID, jobID, "", nil, c.status)
	return err
}
func (c createMoveTargetAttemptCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	if m.intent == nil || m.intent.state != string(MoveIntentStateOpen) || m.intent.targetIdx >= 0 {
		return
	}
	m.prependAttempt(attemptModel{status: c.status, hidden: true, closed: IsTerminalStatus(c.status)})
	m.intent.targetIdx = 0
}

type confirmMoveTargetCmd struct{}

func (c confirmMoveTargetCmd) describe() string { return "ConfirmMoveTargetAccepted" }
func (c confirmMoveTargetCmd) run(db *sql.DB, jobID int64) error {
	intent, err := GetOpenMoveIntent(db, jobID)
	if err != nil {
		return err
	}
	if intent == nil {
		return fmt.Errorf("no open move intent")
	}
	return ConfirmMoveTargetAccepted(db, intent.ID, "state-machine test")
}
func (c confirmMoveTargetCmd) apply(m *jobModel, err error) {
	if err != nil {
		return
	}
	if m.intent == nil || m.intent.state != string(MoveIntentStateOpen) || m.intent.targetIdx < 0 {
		return
	}
	m.attempts[m.intent.sourceIdx].abandoned = true
	m.attempts[m.intent.targetIdx].hidden = false
	m.intent.state = string(MoveIntentStateConfirmed)
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

func randomMoveTargetStatus(rng *rand.Rand) string {
	statuses := []string{StatusQueued, StatusStarting, StatusRunning}
	return statuses[rng.IntN(len(statuses))]
}

// rawAttempt is a test-only view of a DB attempt row, including abandoned ones.
type rawAttempt struct {
	id        int64
	status    string
	closed    bool
	abandoned bool
	hidden    bool
}

func loadRawAttempts(db *sql.DB, jobID int64) ([]rawAttempt, error) {
	rows, err := db.Query(`
		SELECT ja.id, ja.status, ja.end_time IS NOT NULL, ja.abandoned_at IS NOT NULL, COALESCE(mi.state, '')
		FROM job_attempts ja
		LEFT JOIN move_intents mi ON mi.id = ja.move_intent_id
		WHERE ja.job_id = ?
		ORDER BY ja.attempt_number DESC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []rawAttempt
	for rows.Next() {
		var a rawAttempt
		var state string
		if err := rows.Scan(&a.id, &a.status, &a.closed, &a.abandoned, &state); err != nil {
			return nil, err
		}
		a.hidden = state == string(MoveIntentStateOpen)
		out = append(out, a)
	}
	return out, rows.Err()
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

	if job.Status != m.expectedStatus() {
		t.Fatalf("iteration %d step %d: job status = %q, want %q\nmodel=%+v\nseed=%d command=%s",
			iteration, step, job.Status, m.expectedStatus(), m, seed, desc)
	}
	if job.LastSyncedStatus != m.expectedLastSynced() {
		t.Fatalf("iteration %d step %d: last_synced_status = %q, want %q\nmodel=%+v\nseed=%d command=%s",
			iteration, step, job.LastSyncedStatus, m.expectedLastSynced(), m, seed, desc)
	}

	raw, err := loadRawAttempts(db, jobID)
	if err != nil {
		t.Fatalf("iteration %d step %d: loadRawAttempts: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	if len(raw) != len(m.attempts) {
		t.Fatalf("iteration %d step %d: attempt count = %d, want %d\nraw=%+v\nmodel=%+v\nseed=%d command=%s",
			iteration, step, len(raw), len(m.attempts), raw, m.attempts, seed, desc)
	}
	for i := range raw {
		if raw[i].status != m.attempts[i].status {
			t.Fatalf("iteration %d step %d: attempt %d status = %q, want %q\nseed=%d command=%s",
				iteration, step, i, raw[i].status, m.attempts[i].status, seed, desc)
		}
		if raw[i].closed != m.attempts[i].closed {
			t.Fatalf("iteration %d step %d: attempt %d closed = %v, want %v\nseed=%d command=%s",
				iteration, step, i, raw[i].closed, m.attempts[i].closed, seed, desc)
		}
		if raw[i].abandoned != m.attempts[i].abandoned {
			t.Fatalf("iteration %d step %d: attempt %d abandoned = %v, want %v\nseed=%d command=%s",
				iteration, step, i, raw[i].abandoned, m.attempts[i].abandoned, seed, desc)
		}
		if raw[i].hidden != m.attempts[i].hidden {
			t.Fatalf("iteration %d step %d: attempt %d hidden = %v, want %v\nseed=%d command=%s",
				iteration, step, i, raw[i].hidden, m.attempts[i].hidden, seed, desc)
		}
	}

	attempts, err := ListAttempts(db, jobID)
	if err != nil {
		t.Fatalf("iteration %d step %d: ListAttempts: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	t.Logf("iteration %d step %d: command=%s model=%+v attempts=%+v job=%+v", iteration, step, desc, m, attempts, job)

	for i, a := range attempts {
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

	authID, err := GetAuthoritativeAttemptID(db, jobID)
	if err != nil {
		t.Fatalf("iteration %d step %d: GetAuthoritativeAttemptID: %v\nseed=%d command=%s", iteration, step, err, seed, desc)
	}
	authIdx := m.authIdx()
	if authIdx < 0 {
		if authID != 0 {
			t.Fatalf("iteration %d step %d: no authoritative attempt but id = %d\nseed=%d command=%s",
				iteration, step, authID, seed, desc)
		}
	} else {
		if authID != raw[authIdx].id {
			t.Fatalf("iteration %d step %d: authoritative attempt = %d, want %d\nseed=%d command=%s",
				iteration, step, authID, raw[authIdx].id, seed, desc)
		}
		if job.LatestRunID == nil || *job.LatestRunID != authID {
			t.Fatalf("iteration %d step %d: latest_run_id = %v, want %d\nseed=%d command=%s",
				iteration, step, job.LatestRunID, authID, seed, desc)
		}
	}

}

// TestJobAttemptStateMachine_ConcurrentCompletionRace is an example of a
// command interleaving the randomized test cannot schedule deliberately: two
// cloud sync processes observe the same running attempt and both try to record
// completion. RecordCloudJobCompletionWithTransition retries on lock contention
// and reports exactly one transition, leaving a single terminal attempt rather
// than duplicated or corrupted state.
func TestJobAttemptStateMachine_ConcurrentCompletionRace(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "host", "/tmp/project", "echo test", "sm-concurrent")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}

	now := time.Now().Unix()
	type result struct {
		transitioned bool
		err          error
	}
	ch := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			res, err := RecordCloudJobCompletionWithTransition(database, jobID, 0, now-10, now, "", "", time.Time{}, 0)
			ch <- result{res.Transitioned, err}
		}()
	}

	var transitionedCount int
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			t.Fatalf("RecordCloudJobCompletionWithTransition: %v", r.err)
		}
		if r.transitioned {
			transitionedCount++
		}
	}

	if transitionedCount != 1 {
		t.Fatalf("transitionedCount = %d, want 1", transitionedCount)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("status = %q, want %q", job.Status, StatusCompleted)
	}

	attempts, err := ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	if attempts[0].Status != StatusCompleted {
		t.Fatalf("attempt status = %q, want %q", attempts[0].Status, StatusCompleted)
	}
}

// concurrentMoveIntentRace runs a pair of operations against the same job in
// parallel and checks that the resulting DB state is one of the expected
// linearized outcomes. It retries the race up to maxTries so both orderings are
// likely to be exercised on SQLite.
func concurrentMoveIntentRace(t *testing.T, maxTries int, setup func(*sql.DB) int64, op1, op2 func(*sql.DB, int64) error, allowed func(t *testing.T, db *sql.DB, jobID int64, err1, err2 error)) {
	t.Helper()
	for try := 0; try < maxTries; try++ {
		database := SetupTestDB(t)
		jobID := setup(database)

		var err1, err2 error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			err1 = op1(database, jobID)
		}()
		go func() {
			defer wg.Done()
			err2 = op2(database, jobID)
		}()
		wg.Wait()

		allowed(t, database, jobID, err1, err2)
	}
}

// moveAttemptRow is a test-only view of an attempt plus its binding intent.
type moveAttemptRow struct {
	id        int64
	status    string
	closed    bool
	abandoned bool
	intentID  sql.NullInt64
	state     string
}

// loadMoveAttempts returns all attempts for a job with their bound intent state.
func loadMoveAttempts(t *testing.T, db *sql.DB, jobID int64) []moveAttemptRow {
	t.Helper()
	rows, err := db.Query(`
		SELECT ja.id, ja.status, ja.end_time IS NOT NULL, ja.abandoned_at IS NOT NULL, ja.move_intent_id, COALESCE(mi.state, '')
		FROM job_attempts ja
		LEFT JOIN move_intents mi ON mi.id = ja.move_intent_id
		WHERE ja.job_id = ?
		ORDER BY ja.attempt_number DESC`, jobID)
	if err != nil {
		t.Fatalf("loadMoveAttempts query: %v", err)
	}
	defer rows.Close()

	var out []moveAttemptRow
	for rows.Next() {
		var r moveAttemptRow
		if err := rows.Scan(&r.id, &r.status, &r.closed, &r.abandoned, &r.intentID, &r.state); err != nil {
			t.Fatalf("loadMoveAttempts scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("loadMoveAttempts rows: %v", err)
	}
	return out
}

// assertNoCorruptMoveState checks basic invariants that must hold after any
// concurrent move-intent interleaving.
func assertNoCorruptMoveState(t *testing.T, db *sql.DB, jobID int64) {
	t.Helper()
	attempts := loadMoveAttempts(t, db, jobID)

	var openIntents int
	intentByID := make(map[int64]string)
	for _, a := range attempts {
		if a.intentID.Valid {
			intentByID[a.intentID.Int64] = a.state
			if a.state == string(MoveIntentStateOpen) {
				openIntents++
				if !a.abandoned {
					// hidden (open-move-target) attempts are bound to an open intent
					// and must be the only non-abandoned attempt tied to that intent.
				} else {
					t.Fatalf("attempt %d is bound to open intent %d but abandoned", a.id, a.intentID.Int64)
				}
			}
		}
	}
	if openIntents > 1 {
		t.Fatalf("job has %d open move intents, want at most 1", openIntents)
	}

	// job_status view must be derivable from non-abandoned attempts.
	job, err := GetJobByID(db, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil {
		t.Fatalf("job %d missing", jobID)
	}
	var foundAuth bool
	for _, a := range attempts {
		if !a.abandoned && a.state != string(MoveIntentStateOpen) {
			if a.status == job.Status || (a.closed && !IsTerminalStatus(a.status) && job.Status == StatusDead) {
				foundAuth = true
			}
		}
	}
	if !foundAuth {
		// The only legitimate case where no attempt directly matches the job
		// status is when the view falls back to StatusDraft because there is no
		// authoritative attempt, or when a user-level requested_status override
		// is in play.
		var requestedStatus sql.NullString
		if err := db.QueryRow(`SELECT requested_status FROM jobs WHERE id = ?`, jobID).Scan(&requestedStatus); err != nil {
			t.Fatalf("read requested_status: %v", err)
		}
		if job.Status != StatusDraft && !requestedStatus.Valid {
			t.Fatalf("job status %q has no matching authoritative attempt (attempts=%+v)", job.Status, attempts)
		}
	}
}

// TestJobAttemptStateMachine_ConcurrentAbandonVsMoveIntent races abandoning the
// authoritative attempt against creating a move intent from it. Either ordering
// is valid; the invariant is that the job never ends up with an open intent
// whose source was silently abandoned before the intent resolved.
func TestJobAttemptStateMachine_ConcurrentAbandonVsMoveIntent(t *testing.T) {
	setup := func(db *sql.DB) int64 {
		jobID, err := RecordQueued(db, "host", "/tmp/project", "echo test", "sm-abandon-vs-intent")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := MarkQueuedJobRunning(db, jobID); err != nil {
			t.Fatalf("MarkQueuedJobRunning: %v", err)
		}
		return jobID
	}
	abandon := func(db *sql.DB, jobID int64) error {
		attemptID, err := GetAuthoritativeAttemptID(db, jobID)
		if err != nil {
			return err
		}
		if attemptID == 0 {
			return fmt.Errorf("no authoritative attempt")
		}
		return AbandonAttempt(db, attemptID, "concurrent abandon", nil)
	}
	createIntent := func(db *sql.DB, jobID int64) error {
		_, err := CreateMoveIntent(db, CreateMoveIntentParams{
			JobID:      jobID,
			TargetKind: MoveTargetExisting,
			TargetHost: "target-host",
		})
		return err
	}

	concurrentMoveIntentRace(t, 50, setup, abandon, createIntent, func(t *testing.T, db *sql.DB, jobID int64, err1, err2 error) {
		assertNoCorruptMoveState(t, db, jobID)
		intent, _ := GetOpenMoveIntent(db, jobID)
		if intent != nil && intent.SourceAttemptID != nil {
			rows := loadMoveAttempts(t, db, jobID)
			for _, r := range rows {
				if r.id == *intent.SourceAttemptID && r.abandoned {
					t.Fatalf("open intent %d source attempt %d is abandoned", intent.ID, r.id)
				}
			}
		}
	})
}

// TestJobAttemptStateMachine_ConcurrentIntentVsSourceCompletion races creating
// a move intent against completing the source attempt. If completion wins, the
// intent must not remain open with a stale source. If intent wins, the source
// completion must obsolete the intent via trigger.
func TestJobAttemptStateMachine_ConcurrentIntentVsSourceCompletion(t *testing.T) {
	setup := func(db *sql.DB) int64 {
		jobID, err := RecordQueued(db, "host", "/tmp/project", "echo test", "sm-intent-vs-completion")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := MarkQueuedJobRunning(db, jobID); err != nil {
			t.Fatalf("MarkQueuedJobRunning: %v", err)
		}
		return jobID
	}
	createIntent := func(db *sql.DB, jobID int64) error {
		_, err := CreateMoveIntent(db, CreateMoveIntentParams{
			JobID:      jobID,
			TargetKind: MoveTargetExisting,
			TargetHost: "target-host",
		})
		return err
	}
	completeSource := func(db *sql.DB, jobID int64) error {
		now := time.Now().Unix()
		_, err := RecordCloudJobCompletionWithTransition(db, jobID, 0, now-10, now, "", "", time.Time{}, 0)
		return err
	}

	concurrentMoveIntentRace(t, 50, setup, createIntent, completeSource, func(t *testing.T, db *sql.DB, jobID int64, err1, err2 error) {
		assertNoCorruptMoveState(t, db, jobID)
		intent, _ := GetOpenMoveIntent(db, jobID)
		if intent != nil {
			// Intent won the race and is still open; source completion should not
			// have succeeded, or the source is not terminal.
			job, _ := GetJobByID(db, jobID)
			if job.Status == StatusCompleted {
				t.Fatalf("job completed but move intent %d is still open", intent.ID)
			}
		}
	})
}

// TestJobAttemptStateMachine_ConcurrentTargetAttemptVsSourceCompletion races
// creating the hidden target attempt against completing the source. The target
// must not become authoritative if the source wins; if the target wins first,
// source completion must obsolete the intent and abandon the target.
func TestJobAttemptStateMachine_ConcurrentTargetAttemptVsSourceCompletion(t *testing.T) {
	setup := func(db *sql.DB) int64 {
		jobID, err := RecordQueued(db, "host", "/tmp/project", "echo test", "sm-target-vs-completion")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := MarkQueuedJobRunning(db, jobID); err != nil {
			t.Fatalf("MarkQueuedJobRunning: %v", err)
		}
		_, err = CreateMoveIntent(db, CreateMoveIntentParams{
			JobID:      jobID,
			TargetKind: MoveTargetExisting,
			TargetHost: "target-host",
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		return jobID
	}
	createTarget := func(db *sql.DB, jobID int64) error {
		intent, err := GetOpenMoveIntent(db, jobID)
		if err != nil {
			return err
		}
		if intent == nil {
			return fmt.Errorf("no open move intent")
		}
		_, err = CreateMoveTargetAttempt(db, intent.ID, jobID, "target-host", nil, StatusQueued)
		return err
	}
	completeSource := func(db *sql.DB, jobID int64) error {
		now := time.Now().Unix()
		_, err := RecordCloudJobCompletionWithTransition(db, jobID, 0, now-10, now, "", "", time.Time{}, 0)
		return err
	}

	concurrentMoveIntentRace(t, 50, setup, createTarget, completeSource, func(t *testing.T, db *sql.DB, jobID int64, err1, err2 error) {
		assertNoCorruptMoveState(t, db, jobID)
		// If a hidden target exists bound to an open intent, the source cannot be
		// terminal.
		attempts := loadMoveAttempts(t, db, jobID)
		for _, a := range attempts {
			if a.state == string(MoveIntentStateOpen) && a.status == StatusQueued && !a.abandoned {
				job, _ := GetJobByID(db, jobID)
				if job.Status == StatusCompleted {
					t.Fatalf("hidden target exists but job is completed")
				}
			}
		}
	})
}

// TestJobAttemptStateMachine_ConcurrentConfirmVsSourceCompletion races
// confirming the move target against completing the source attempt. Only one
// path can decide the job outcome: either the target is confirmed and the job
// moves to the target, or the source completes and the intent is obsoleted.
func TestJobAttemptStateMachine_ConcurrentConfirmVsSourceCompletion(t *testing.T) {
	setup := func(db *sql.DB) int64 {
		jobID, err := RecordQueued(db, "host", "/tmp/project", "echo test", "sm-confirm-vs-completion")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := MarkQueuedJobRunning(db, jobID); err != nil {
			t.Fatalf("MarkQueuedJobRunning: %v", err)
		}
		_, err = CreateMoveIntent(db, CreateMoveIntentParams{
			JobID:      jobID,
			TargetKind: MoveTargetExisting,
			TargetHost: "target-host",
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		intent, err := GetOpenMoveIntent(db, jobID)
		if err != nil {
			t.Fatalf("GetOpenMoveIntent: %v", err)
		}
		_, err = CreateMoveTargetAttempt(db, intent.ID, jobID, "target-host", nil, StatusQueued)
		if err != nil {
			t.Fatalf("CreateMoveTargetAttempt: %v", err)
		}
		return jobID
	}
	confirm := func(db *sql.DB, jobID int64) error {
		intent, err := GetOpenMoveIntent(db, jobID)
		if err != nil {
			return err
		}
		if intent == nil {
			return fmt.Errorf("no open move intent")
		}
		return ConfirmMoveTargetAccepted(db, intent.ID, "state-machine test")
	}
	completeSource := func(db *sql.DB, jobID int64) error {
		now := time.Now().Unix()
		_, err := RecordCloudJobCompletionWithTransition(db, jobID, 0, now-10, now, "", "", time.Time{}, 0)
		return err
	}

	concurrentMoveIntentRace(t, 50, setup, confirm, completeSource, func(t *testing.T, db *sql.DB, jobID int64, err1, err2 error) {
		assertNoCorruptMoveState(t, db, jobID)
		intent, _ := GetOpenMoveIntent(db, jobID)
		if intent != nil {
			t.Fatalf("move intent %d still open after confirm-vs-completion race", intent.ID)
		}
	})
}

// TestJobAttemptStateMachine_ConcurrentConfirmVsTargetCompletion races
// confirming the move target against the target attempt becoming terminal. If
// the target completes first, the auto-confirm trigger promotes it. If confirm
// wins first, the target is authoritative and may then complete normally.
func TestJobAttemptStateMachine_ConcurrentConfirmVsTargetCompletion(t *testing.T) {
	setup := func(db *sql.DB) int64 {
		jobID, err := RecordQueued(db, "host", "/tmp/project", "echo test", "sm-confirm-vs-target")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := MarkQueuedJobRunning(db, jobID); err != nil {
			t.Fatalf("MarkQueuedJobRunning: %v", err)
		}
		_, err = CreateMoveIntent(db, CreateMoveIntentParams{
			JobID:      jobID,
			TargetKind: MoveTargetExisting,
			TargetHost: "target-host",
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		intent, err := GetOpenMoveIntent(db, jobID)
		if err != nil {
			t.Fatalf("GetOpenMoveIntent: %v", err)
		}
		_, err = CreateMoveTargetAttempt(db, intent.ID, jobID, "target-host", nil, StatusQueued)
		if err != nil {
			t.Fatalf("CreateMoveTargetAttempt: %v", err)
		}
		return jobID
	}
	confirm := func(db *sql.DB, jobID int64) error {
		intent, err := GetOpenMoveIntent(db, jobID)
		if err != nil {
			return err
		}
		if intent == nil {
			return fmt.Errorf("no open move intent")
		}
		return ConfirmMoveTargetAccepted(db, intent.ID, "state-machine test")
	}
	completeTarget := func(db *sql.DB, jobID int64) error {
		intent, err := GetOpenMoveIntent(db, jobID)
		if err != nil {
			return err
		}
		if intent == nil || intent.TargetAttemptID == nil {
			return fmt.Errorf("no target attempt")
		}
		now := time.Now().Unix()
		return CloseAttempt(db, jobID, StatusCompleted, intPtr(0), now)
	}

	concurrentMoveIntentRace(t, 50, setup, confirm, completeTarget, func(t *testing.T, db *sql.DB, jobID int64, err1, err2 error) {
		assertNoCorruptMoveState(t, db, jobID)
		intent, _ := GetOpenMoveIntent(db, jobID)
		if intent != nil {
			t.Fatalf("move intent %d still open after confirm-vs-target race", intent.ID)
		}
	})
}

func jobAttemptSMSeed() uint64 {
	if s := os.Getenv("WEFT_TEST_SEED"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			return n
		}
	}
	return rand.Uint64()
}
