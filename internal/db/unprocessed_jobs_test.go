package db

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

const inboxTestSession = "session-inbox-test"

// inboxTestSeed records seeded jobs by expected fate in the unprocessed
// terminal-job inbox.
type inboxTestSeed struct {
	inbox    map[int64]string // expected in the loader result: id -> label
	excluded map[int64]string // dropped by the SQL pre-filter already
	goOnly   map[int64]string // survive the SQL pre-filter; the Go tag filter drops them
}

func (s inboxTestSeed) all() map[int64]string {
	out := make(map[int64]string, len(s.inbox)+len(s.excluded)+len(s.goOnly))
	for id, label := range s.inbox {
		out[id] = label
	}
	for id, label := range s.excluded {
		out[id] = label
	}
	for id, label := range s.goOnly {
		out[id] = label
	}
	return out
}

// seedUnprocessedInboxTestJobs seeds jobs covering every status constant,
// pending_status in both directions, processed-tag encodings and near-miss
// tags, the 14-day window boundary, and tombstoning.
func seedUnprocessedInboxTestJobs(t *testing.T, database *sql.DB) inboxTestSeed {
	t.Helper()
	now := time.Now().Unix()
	exitZero := 0
	exitOne := 1
	seed := inboxTestSeed{
		inbox:    map[int64]string{},
		excluded: map[int64]string{},
		goOnly:   map[int64]string{},
	}

	queued := func() int64 {
		t.Helper()
		id, err := RecordQueued(database, "cool30", "/tmp/inbox", "echo ok", "inbox test")
		if err != nil {
			t.Fatalf("record queued: %v", err)
		}
		if err := SetJobSubmitterSession(database, id, inboxTestSession); err != nil {
			t.Fatalf("set submitter session: %v", err)
		}
		return id
	}
	terminal := func(status string, exitCode *int) int64 {
		t.Helper()
		id := queued()
		if err := CloseAttempt(database, id, status, exitCode, now); err != nil {
			t.Fatalf("close %s: %v", status, err)
		}
		return id
	}
	requestTerminal := func(status string) int64 {
		t.Helper()
		id := queued()
		if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, status, now, id); err != nil {
			t.Fatalf("mark %s: %v", status, err)
		}
		if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, status, id); err != nil {
			t.Fatalf("request %s: %v", status, err)
		}
		return id
	}
	withStatus := func(status string) int64 {
		t.Helper()
		id := queued()
		if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ?`, status, id); err != nil {
			t.Fatalf("set status %s: %v", status, err)
		}
		return id
	}
	setPending := func(id int64, pending string) {
		t.Helper()
		if _, err := database.Exec(`UPDATE job_attempts SET pending_status = ? WHERE job_id = ?`, pending, id); err != nil {
			t.Fatalf("set pending %s: %v", pending, err)
		}
	}
	setRawTags := func(id int64, raw string) {
		t.Helper()
		if _, err := database.Exec(`UPDATE jobs SET tags = ? WHERE id = ?`, raw, id); err != nil {
			t.Fatalf("set raw tags %q: %v", raw, err)
		}
	}
	tag := func(id int64, tag string) {
		t.Helper()
		if err := AddJobTag(database, id, tag); err != nil {
			t.Fatalf("add tag %q: %v", tag, err)
		}
	}

	// Inbox rows: effective status completed/failed/dead, not processed.
	seed.inbox[terminal(StatusCompleted, &exitZero)] = "completed (null start_time)"
	seed.inbox[terminal(StatusFailed, &exitOne)] = "failed"
	seed.inbox[terminal(StatusDead, nil)] = "dead"

	withTags := terminal(StatusCompleted, &exitZero)
	tag(withTags, "unprocessed")
	seed.inbox[withTags] = `tag "unprocessed" (near-miss)`
	withTags = terminal(StatusCompleted, &exitZero)
	tag(withTags, "reprocessed")
	seed.inbox[withTags] = `tag "reprocessed" (near-miss)`
	withTags = terminal(StatusCompleted, &exitZero)
	tag(withTags, "processed-by-x")
	seed.inbox[withTags] = `tag "processed-by-x" (near-miss)`
	withTags = terminal(StatusCompleted, &exitZero)
	setRawTags(withTags, "foo,bar")
	seed.inbox[withTags] = "legacy comma tags without processed"
	withTags = terminal(StatusCompleted, &exitZero)
	setRawTags(withTags, `["x\"processed"]`)
	seed.inbox[withTags] = `tag containing an escaped quote before "processed"`

	pendingCompleted := queued()
	if err := UpdateQueuedToRunning(database, pendingCompleted); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	setPending(pendingCompleted, StatusCompleted)
	seed.inbox[pendingCompleted] = "running with pending_status=completed"
	pendingFailed := queued()
	setPending(pendingFailed, StatusFailed)
	seed.inbox[pendingFailed] = "queued with pending_status=failed"
	conflicting := terminal(StatusCompleted, &exitZero)
	setPending(conflicting, StatusRunning)
	seed.inbox[conflicting] = "completed with pending_status=running (terminal wins)"

	inWindow := terminal(StatusCompleted, &exitZero)
	if _, err := database.Exec(`UPDATE job_attempts SET start_time = ?, end_time = ? WHERE job_id = ?`, now-86400, now-86400, inWindow); err != nil {
		t.Fatalf("set in-window attempt times: %v", err)
	}
	seed.inbox[inWindow] = "completed one day ago"

	// SQL-excluded rows.
	seed.excluded[requestTerminal(StatusKilled)] = "killed (terminal, not inbox)"
	seed.excluded[requestTerminal(StatusCanceled)] = "canceled (terminal, not inbox)"
	running := queued()
	if err := UpdateQueuedToRunning(database, running); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	seed.excluded[running] = "running"
	seed.excluded[queued()] = "queued"
	seed.excluded[withStatus(StatusStarting)] = "starting"
	seed.excluded[withStatus(StatusPaused)] = "paused"
	seed.excluded[withStatus(StatusPendingPlacement)] = "pending_placement"
	res, err := database.Exec(`INSERT INTO jobs (working_dir, command, created_at, tombstoned) VALUES ('/tmp/inbox', 'echo ok', ?, 0)`, now)
	if err != nil {
		t.Fatalf("insert draft job: %v", err)
	}
	draftID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("draft job id: %v", err)
	}
	if err := SetJobSubmitterSession(database, draftID, inboxTestSession); err != nil {
		t.Fatalf("set draft submitter session: %v", err)
	}
	seed.excluded[draftID] = "draft (no attempt)"

	processed := terminal(StatusCompleted, &exitZero)
	tag(processed, ProcessedTag)
	seed.excluded[processed] = "completed with JSON processed tag"
	processed = terminal(StatusFailed, &exitOne)
	tag(processed, ProcessedTag)
	seed.excluded[processed] = "failed with JSON processed tag"

	pendingKilled := queued()
	if err := UpdateQueuedToRunning(database, pendingKilled); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	setPending(pendingKilled, StatusKilled)
	seed.excluded[pendingKilled] = "running with pending_status=killed (effective killed)"
	terminalWins := requestTerminal(StatusKilled)
	setPending(terminalWins, StatusCompleted)
	seed.excluded[terminalWins] = "killed with pending_status=completed (terminal wins)"

	old := terminal(StatusCompleted, &exitZero)
	if _, err := database.Exec(`UPDATE job_attempts SET start_time = ?, end_time = ? WHERE job_id = ?`, now-31*86400, now-31*86400, old); err != nil {
		t.Fatalf("set old attempt times: %v", err)
	}
	seed.excluded[old] = "completed 31 days ago (outside the window)"

	tombstoned := terminal(StatusCompleted, &exitZero)
	if _, err := database.Exec(`UPDATE jobs SET tombstoned = 1 WHERE id = ?`, tombstoned); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	seed.excluded[tombstoned] = "tombstoned"

	// SQL keeps these (superset direction); the Go tag filter drops them.
	legacy := terminal(StatusFailed, &exitOne)
	setRawTags(legacy, "processed,foo")
	seed.goOnly[legacy] = "legacy comma tags carrying processed"
	legacy = terminal(StatusCompleted, &exitZero)
	setRawTags(legacy, "processed")
	seed.goOnly[legacy] = "legacy bare processed tag"

	return seed
}

func sortedIDs(m map[int64]string) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func jobIDs(jobs []*Job) []int64 {
	ids := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func labelIDs(ids []int64, labels map[int64]string) string {
	out := ""
	for _, id := range ids {
		out += fmt.Sprintf("\n  %d %s", id, labels[id])
	}
	return out
}

func equalIDSets(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestListUnprocessedTerminalJobsMatchesGoPredicate is the equivalence guard:
// the SQL-pushed loader must return exactly the jobs the old fetch-everything
// path (ListJobsWithMaxAge + the Go effective-status predicate) returns.
func TestListUnprocessedTerminalJobsMatchesGoPredicate(t *testing.T) {
	database := SetupTestDB(t)
	seed := seedUnprocessedInboxTestJobs(t, database)

	got := jobIDs(mustUnprocessedTerminalJobs(t, database, 14))

	// The old path: fetch the whole window, then apply the Go predicate.
	// isUnprocessedTerminalJob reduces to an effective status of completed,
	// failed, or dead.
	oldJobs, err := ListJobsWithMaxAge(database, "", "", 0, 14, nil, "unprocessed")
	if err != nil {
		t.Fatalf("ListJobsWithMaxAge: %v", err)
	}
	wantSet := map[int64]string{}
	for _, j := range oldJobs {
		switch j.EffectiveStatus() {
		case StatusCompleted, StatusFailed, StatusDead:
			wantSet[j.ID] = seed.all()[j.ID]
		}
	}
	want := sortedIDs(wantSet)
	if !equalIDSets(got, want) {
		t.Fatalf("loader result differs from the old path\ngot:%s\nwant:%s",
			labelIDs(got, seed.all()), labelIDs(want, seed.all()))
	}

	// Anchor the expectation to the seeding, so the test cannot agree with
	// itself on an empty or shifted set.
	seeded := sortedIDs(seed.inbox)
	if !equalIDSets(got, seeded) {
		t.Fatalf("loader result differs from the seeded inbox\ngot:%s\nseeded:%s",
			labelIDs(got, seed.all()), labelIDs(seeded, seed.all()))
	}
}

// TestListUnprocessedTerminalJobsStatusSet pins SQL/Go agreement per status:
// for one clean job per status constant, loader membership must equal the Go
// effective-status predicate, so the SQL predicate cannot drift from
// EffectiveStatus.
func TestListUnprocessedTerminalJobsStatusSet(t *testing.T) {
	database := SetupTestDB(t)
	seed := seedUnprocessedInboxTestJobs(t, database)

	got := map[int64]bool{}
	for _, id := range jobIDs(mustUnprocessedTerminalJobs(t, database, 14)) {
		got[id] = true
	}

	allJobs, err := ListJobsWithMaxAge(database, "", "", 0, 14, nil, "")
	if err != nil {
		t.Fatalf("ListJobsWithMaxAge: %v", err)
	}
	inboxByGo := map[int64]bool{}
	for _, j := range allJobs {
		eff := j.EffectiveStatus()
		terminal := eff == StatusCompleted || eff == StatusFailed || eff == StatusDead
		inboxByGo[j.ID] = terminal && !j.HasTag(ProcessedTag)
	}

	for id, label := range seed.all() {
		want, ok := inboxByGo[id]
		if !ok {
			// Tombstoned and out-of-window jobs are invisible to both paths.
			continue
		}
		if got[id] != want {
			t.Errorf("job %d (%s): loader membership = %v, Go predicate says %v", id, label, got[id], want)
		}
	}
}

// TestUnprocessedTerminalJobsSQLPrefilterDropsNonInboxRows checks the raw SQL
// (before the Go tag filter): the obvious non-inbox rows must already be
// excluded so the row-count win is real, while rows only Go can judge (legacy
// comma-separated tags) must survive — SQL returns a superset, Go narrows it.
func TestUnprocessedTerminalJobsSQLPrefilterDropsNonInboxRows(t *testing.T) {
	database := SetupTestDB(t)
	seed := seedUnprocessedInboxTestJobs(t, database)

	query, args := unprocessedTerminalJobsQuery(14, inboxTestSession)
	rows, err := database.Query(`SELECT id FROM (`+query+`)`, args...)
	if err != nil {
		t.Fatalf("candidate query: %v", err)
	}
	defer rows.Close()
	candidates := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan candidate: %v", err)
		}
		candidates[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate candidates: %v", err)
	}

	for id, label := range seed.inbox {
		if !candidates[id] {
			t.Errorf("SQL pre-filter dropped inbox row %d (%s)", id, label)
		}
	}
	for id, label := range seed.excluded {
		if candidates[id] {
			t.Errorf("SQL pre-filter kept non-inbox row %d (%s)", id, label)
		}
	}
	for id, label := range seed.goOnly {
		if !candidates[id] {
			t.Errorf("SQL pre-filter dropped %d (%s); only the Go filter may drop it", id, label)
		}
	}

	// The row-count win: rows the SQL fetches versus what the old
	// fetch-everything query fetched, and versus the final Go-filtered
	// result.
	cutoff := time.Now().AddDate(0, 0, -14).Unix()
	var oldFetch int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM job_status WHERE tombstoned = 0 AND (start_time > ? OR start_time IS NULL OR start_time = 0)`,
		cutoff).Scan(&oldFetch); err != nil {
		t.Fatalf("count old fetch: %v", err)
	}
	final := mustUnprocessedTerminalJobs(t, database, 14)
	t.Logf("inbox query rows: old fetch = %d, SQL pre-filter = %d, final after Go = %d",
		oldFetch, len(candidates), len(final))
	if len(candidates) >= oldFetch {
		t.Errorf("SQL pre-filter fetched %d rows, old query fetched %d — no row-count win", len(candidates), oldFetch)
	}
	plan := explainQueryPlan(t, database, query, args...)
	t.Logf("plan:\n%s", plan)
	if !strings.Contains(plan, "idx_jobs_submitter_session") {
		t.Errorf("session inbox query does not use idx_jobs_submitter_session:\n%s", plan)
	}
}

func recordCompletedInboxJobWithUnknownAttemptTimes(t *testing.T, database *sql.DB, createdAt int64) int64 {
	t.Helper()
	jobID, err := RecordQueued(database, "cool30", "/tmp/inbox-age", "echo ok", "inbox age")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if err := SetJobSubmitterSession(database, jobID, inboxTestSession); err != nil {
		t.Fatalf("set submitter session: %v", err)
	}
	exitZero := 0
	if err := CloseAttempt(database, jobID, StatusCompleted, &exitZero, time.Now().Unix()); err != nil {
		t.Fatalf("close attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET start_time = NULL, end_time = NULL WHERE job_id = ?`, jobID); err != nil {
		t.Fatalf("clear attempt times: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET created_at = ? WHERE id = ?`, createdAt, jobID); err != nil {
		t.Fatalf("set created_at: %v", err)
	}
	return jobID
}

func TestUnprocessedTerminalJobsAgeUsesAvailableProvenance(t *testing.T) {
	database := SetupTestDB(t)
	oldID := recordCompletedInboxJobWithUnknownAttemptTimes(t, database, time.Now().AddDate(0, 0, -30).Unix())
	unknownID := recordCompletedInboxJobWithUnknownAttemptTimes(t, database, 0)

	result, err := ListUnprocessedTerminalJobs(database, 14, inboxTestSession)
	if err != nil {
		t.Fatalf("list inbox: %v", err)
	}
	foundUnknown := false
	for _, job := range result.Jobs {
		switch job.ID {
		case oldID:
			t.Fatalf("old job %d included despite usable created_at", oldID)
		case unknownID:
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Fatalf("job %d with no usable age timestamp was omitted", unknownID)
	}
}

func TestListUnprocessedTerminalJobsUsesExactSessionScope(t *testing.T) {
	database := SetupTestDB(t)
	exitZero := 0
	now := time.Now().Unix()
	record := func(session string) int64 {
		t.Helper()
		jobID, err := RecordQueued(database, "cool30", "/tmp/session-scope", "echo ok", session)
		if err != nil {
			t.Fatalf("record %q: %v", session, err)
		}
		if session != "" {
			if err := SetJobSubmitterSession(database, jobID, session); err != nil {
				t.Fatalf("set session %q: %v", session, err)
			}
		}
		if err := CloseAttempt(database, jobID, StatusCompleted, &exitZero, now); err != nil {
			t.Fatalf("close %q: %v", session, err)
		}
		return jobID
	}
	wantID := record("session-abc")
	record("session-abc-child")
	record("")

	result, err := ListUnprocessedTerminalJobs(database, 14, "session-abc")
	if err != nil {
		t.Fatalf("ListUnprocessedTerminalJobs: %v", err)
	}
	if !result.Scoped || len(result.Jobs) != 1 || result.Jobs[0].ID != wantID {
		t.Fatalf("exact session result = scoped:%v jobs:%v, want only %d", result.Scoped, jobIDs(result.Jobs), wantID)
	}

	unscoped, err := ListUnprocessedTerminalJobs(database, 14, "")
	if err != nil {
		t.Fatalf("unscoped ListUnprocessedTerminalJobs: %v", err)
	}
	if unscoped.Scoped || len(unscoped.Jobs) != 0 {
		t.Fatalf("missing session result = scoped:%v jobs:%v, want a distinct unscoped empty result", unscoped.Scoped, jobIDs(unscoped.Jobs))
	}
}

func mustUnprocessedTerminalJobs(t *testing.T, database *sql.DB, maxAgeDays int) []*Job {
	t.Helper()
	result, err := ListUnprocessedTerminalJobs(database, maxAgeDays, inboxTestSession)
	if err != nil {
		t.Fatalf("ListUnprocessedTerminalJobs: %v", err)
	}
	if !result.Scoped {
		t.Fatal("ListUnprocessedTerminalJobs returned an unscoped result")
	}
	return result.Jobs
}
