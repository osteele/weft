package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db/migrations"
)

func TestReportBugDedupesOpenFingerprint(t *testing.T) {
	database := SetupTestDB(t)

	first, created, err := ReportBug(database, BugReport{
		Title:       "runner pending job is missing queue payload",
		Scope:       "infrastructure",
		Fingerprint: "queue.missing_payload:studio",
		Detail:      "first",
	})
	if err != nil {
		t.Fatalf("ReportBug first: %v", err)
	}
	if !created {
		t.Fatal("first report should create bug")
	}

	second, created, err := ReportBug(database, BugReport{
		Title:       "runner pending job is missing queue payload",
		Scope:       "infrastructure",
		Fingerprint: "queue.missing_payload:studio",
		Detail:      "second",
	})
	if err != nil {
		t.Fatalf("ReportBug second: %v", err)
	}
	if created {
		t.Fatal("second report should update existing bug")
	}
	if second.ID != first.ID {
		t.Fatalf("second bug id = %d, want %d", second.ID, first.ID)
	}
	if second.Occurrences != 2 {
		t.Fatalf("occurrences = %d, want 2", second.Occurrences)
	}
	if second.Detail != "second" {
		t.Fatalf("detail = %q, want latest detail", second.Detail)
	}
}

func TestReportBugErrorsOnClosedFingerprint(t *testing.T) {
	database := SetupTestDB(t)

	bug, created, err := ReportBug(database, BugReport{
		Title:       "artifact cat cannot read listed path",
		Fingerprint: "artifact.cat.closed",
	})
	if err != nil {
		t.Fatalf("ReportBug first: %v", err)
	}
	if !created {
		t.Fatal("first report should create bug")
	}
	if err := CloseBug(database, bug.ID, "fixed"); err != nil {
		t.Fatalf("CloseBug: %v", err)
	}

	_, _, err = ReportBug(database, BugReport{
		Title:       "artifact cat cannot read listed path",
		Fingerprint: "artifact.cat.closed",
	})
	if err == nil {
		t.Fatal("expected closed fingerprint error")
	}
	if got := err.Error(); !strings.Contains(got, "weft bug reopen wb1") || !strings.Contains(got, "different --fingerprint") {
		t.Fatalf("error = %q, want reopen guidance", got)
	}
}

func TestReportBugAgainstClosedFingerprintRecordsRecurrence(t *testing.T) {
	// Kills the mutation that returns the closed-fingerprint error without
	// recording the occurrence (recurrences stays 0), the one that reopens the
	// bug on recurrence (status becomes open), and the one that inflates
	// Occurrences instead of counting post-close reports separately.
	database := SetupTestDB(t)

	bug, _, err := ReportBug(database, BugReport{
		Title:       "runner pending job is missing queue payload",
		Fingerprint: "queue.missing_payload:studio",
		Host:        "host-alpha",
	})
	if err != nil {
		t.Fatalf("ReportBug first: %v", err)
	}
	if err := CloseBug(database, bug.ID, "fixed"); err != nil {
		t.Fatalf("CloseBug: %v", err)
	}

	jobID := int64(4242)
	insertTestJob(t, database, jobID, "", "", "queued")
	for i := 0; i < 3; i++ {
		_, _, err := ReportBug(database, BugReport{
			Title:       "runner pending job is missing queue payload",
			Fingerprint: "queue.missing_payload:studio",
			JobID:       &jobID,
			Host:        "host-beta",
			Note:        "observed again",
		})
		if err == nil {
			t.Fatalf("report %d: expected closed-fingerprint error", i+1)
		}
		if !IsBugFingerprintClosed(err) {
			t.Fatalf("report %d: error %v is not a closed-fingerprint error", i+1, err)
		}
		if !strings.Contains(err.Error(), "recurrence recorded (") {
			t.Fatalf("report %d: error %q does not state that the recurrence was recorded", i+1, err)
		}
	}

	got, err := GetBug(database, bug.ID)
	if err != nil {
		t.Fatalf("GetBug: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed: a recurrence must not overturn the close verdict", got.Status)
	}
	if got.Occurrences != 1 {
		t.Fatalf("occurrences = %d, want 1: post-close reports must not inflate the pre-close count", got.Occurrences)
	}
	if got.Recurrences != 3 {
		t.Fatalf("recurrences = %d, want 3", got.Recurrences)
	}
	if got.LastRecurrenceAt == 0 {
		t.Fatal("last_recurrence_at = 0, want the most recent post-close report time")
	}
	if got.Host != "host-alpha" {
		t.Fatalf("host = %q, want the original host kept", got.Host)
	}
	if got.JobID == nil || *got.JobID != jobID {
		t.Fatalf("job id = %v, want %d filled in from the recurrence report", got.JobID, jobID)
	}
	notes, err := ListBugNotes(database, bug.ID)
	if err != nil {
		t.Fatalf("ListBugNotes: %v", err)
	}
	if len(notes) != 3 || notes[2].Body != "observed again" {
		t.Fatalf("notes = %d, want 3 recurrence notes", len(notes))
	}
}

func TestCloseBugStartsANewRecurrenceInterval(t *testing.T) {
	// Kills the mutation that leaves recurrences and last_recurrence_at intact
	// across a close. Recurrence counters describe the interval since the
	// current close; carrying them over resurfaces a freshly closed bug in the
	// default listing with nothing having happened since that close.
	database := SetupTestDB(t)

	bug, _, err := ReportBug(database, BugReport{
		Title:       "runner pending job is missing queue payload",
		Fingerprint: "queue.missing_payload:studio",
		Host:        "host-alpha",
	})
	if err != nil {
		t.Fatalf("ReportBug: %v", err)
	}
	if err := CloseBug(database, bug.ID, "fixed"); err != nil {
		t.Fatalf("CloseBug first: %v", err)
	}
	// Two recurrences arrive against that first closed interval.
	for i := 0; i < 2; i++ {
		if _, _, err := ReportBug(database, BugReport{
			Title:       "runner pending job is missing queue payload",
			Fingerprint: "queue.missing_payload:studio",
			Host:        "host-alpha",
		}); !IsBugFingerprintClosed(err) {
			t.Fatalf("recurrence %d: err = %v, want closed-fingerprint error", i, err)
		}
	}
	if err := ReopenBug(database, bug.ID); err != nil {
		t.Fatalf("ReopenBug: %v", err)
	}
	if err := CloseBug(database, bug.ID, "fixed properly"); err != nil {
		t.Fatalf("CloseBug second: %v", err)
	}

	got, err := GetBug(database, bug.ID)
	if err != nil {
		t.Fatalf("GetBug: %v", err)
	}
	if got.Recurrences != 0 {
		t.Fatalf("recurrences = %d, want 0: a new close starts a new interval, and nothing has recurred since it", got.Recurrences)
	}
	if got.LastRecurrenceAt != 0 {
		t.Fatalf("last_recurrence_at = %d, want 0: a stale timestamp resurfaces a freshly closed bug", got.LastRecurrenceAt)
	}
}

func TestListBugsSurfacesClosedWithRecentRecurrences(t *testing.T) {
	// Kills the mutation that filters every closed bug out of the default
	// list, the one that drops the count floor (a single straggler then
	// resurfaces the bug), and the one that drops the recency floor (a stale
	// recurrence history then resurfaces it forever).
	database := SetupTestDB(t)

	seed := func(title, fingerprint string) *Bug {
		t.Helper()
		bug, _, err := ReportBug(database, BugReport{Title: title, Fingerprint: fingerprint})
		if err != nil {
			t.Fatalf("ReportBug %s: %v", title, err)
		}
		if err := CloseBug(database, bug.ID, "fixed"); err != nil {
			t.Fatalf("CloseBug %s: %v", title, err)
		}
		return bug
	}
	setRecurrences := func(id int64, count, lastRecurrenceAt int64) {
		t.Helper()
		if _, err := database.Exec(`UPDATE bugs SET recurrences = ?, last_recurrence_at = ? WHERE id = ?`, count, lastRecurrenceAt, id); err != nil {
			t.Fatalf("seed recurrences: %v", err)
		}
	}

	openBug, _, err := ReportBug(database, BugReport{Title: "still open", Fingerprint: "floor.open"})
	if err != nil {
		t.Fatalf("ReportBug open: %v", err)
	}
	straggler := seed("single straggler", "floor.straggler")
	ongoing := seed("ongoing recurrence", "floor.ongoing")
	stale := seed("stale recurrence", "floor.stale")

	now := time.Now().Unix()
	setRecurrences(straggler.ID, 1, now)
	setRecurrences(ongoing.ID, 2, now)
	setRecurrences(stale.ID, 5, now-30*24*int64(time.Hour))

	defaultBugs, err := ListBugs(database, false)
	if err != nil {
		t.Fatalf("ListBugs default: %v", err)
	}
	ids := map[int64]bool{}
	for _, bug := range defaultBugs {
		ids[bug.ID] = true
	}
	if !ids[openBug.ID] {
		t.Fatal("default list dropped an open bug")
	}
	if ids[straggler.ID] {
		t.Fatal("default list surfaced a closed bug with a single recent recurrence")
	}
	if !ids[ongoing.ID] {
		t.Fatal("default list did not surface a closed bug with recent repeated recurrences")
	}
	if ids[stale.ID] {
		t.Fatal("default list surfaced a closed bug whose most recent recurrence is a month old")
	}

	allBugs, err := ListBugs(database, true)
	if err != nil {
		t.Fatalf("ListBugs --all: %v", err)
	}
	if len(allBugs) != 4 {
		t.Fatalf("len(--all) = %d, want 4", len(allBugs))
	}
}

func TestBugRecurrenceMigrationAddsColumns(t *testing.T) {
	// Kills the mutation that removes or misnumbers the bugs recurrence
	// migration: opening a one-version-behind database must apply it and
	// produce the recurrence columns.
	path := filepath.Join(t.TempDir(), "jobs.db")
	seedPendingMigrationTestDBFile(t, path)
	restorePath := SetDBPath(path)
	t.Cleanup(restorePath)

	database, err := Open()
	if err != nil {
		t.Fatalf("Open pending-migration database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	version := migrations.Version(t.Context(), database)
	if version != migrations.Target() {
		t.Fatalf("version = %d, want %d", version, migrations.Target())
	}
	for _, col := range bugColumnDDLs() {
		present, err := bugColumnPresent(database, col[0])
		if err != nil {
			t.Fatalf("check column %s after migration: %v", col[0], err)
		}
		if !present {
			t.Fatalf("column %s missing after applying the last migration", col[0])
		}
	}
}

func TestOpenBugDBAddsRecurrenceColumnsToExistingBugDB(t *testing.T) {
	// Kills the mutation that drops the idempotent column ensure from
	// OpenBugDB: a bugs.db written before recurrence tracking must still open,
	// keep its rows, and record recurrences.
	dir := t.TempDir()
	bugPath := filepath.Join(dir, "bugs.db")
	writePreRecurrenceBugDB(t, bugPath)

	restoreJobsPath := SetDBPath(filepath.Join(dir, "absent-jobs.db"))
	t.Cleanup(restoreJobsPath)
	restoreBugPath := SetBugDBPath(bugPath)
	t.Cleanup(restoreBugPath)

	database, err := OpenBugDB()
	if err != nil {
		t.Fatalf("OpenBugDB on pre-recurrence bugs.db: %v", err)
	}
	defer database.Close()

	for _, col := range bugColumnDDLs() {
		present, err := bugColumnPresent(database, col[0])
		if err != nil {
			t.Fatalf("check column %s: %v", col[0], err)
		}
		if !present {
			t.Fatalf("column %s missing after open", col[0])
		}
	}
	bugs, err := ListBugs(database, true)
	if err != nil {
		t.Fatalf("ListBugs: %v", err)
	}
	if len(bugs) != 1 || bugs[0].Title != "pre-recurrence invariant" {
		t.Fatalf("existing rows not preserved: %+v", bugs)
	}

	if err := CloseBug(database, bugs[0].ID, "fixed"); err != nil {
		t.Fatalf("CloseBug: %v", err)
	}
	_, _, err = ReportBug(database, BugReport{Title: "pre-recurrence invariant", Fingerprint: "legacy.closed.fingerprint"})
	if !IsBugFingerprintClosed(err) {
		t.Fatalf("report against migrated closed bug: err = %v, want closed-fingerprint error", err)
	}
	got, err := GetBug(database, bugs[0].ID)
	if err != nil {
		t.Fatalf("GetBug: %v", err)
	}
	if got.Recurrences != 1 {
		t.Fatalf("recurrences = %d, want 1 on a database migrated at open time", got.Recurrences)
	}
}

func TestReopenBug(t *testing.T) {
	database := SetupTestDB(t)

	bug, _, err := ReportBug(database, BugReport{
		Title:       "artifact cat cannot read listed path",
		Fingerprint: "artifact.cat.reopen",
	})
	if err != nil {
		t.Fatalf("ReportBug: %v", err)
	}
	if err := CloseBug(database, bug.ID, "fixed"); err != nil {
		t.Fatalf("CloseBug: %v", err)
	}
	if err := ReopenBug(database, bug.ID); err != nil {
		t.Fatalf("ReopenBug: %v", err)
	}

	reopened, err := GetBug(database, bug.ID)
	if err != nil {
		t.Fatalf("GetBug: %v", err)
	}
	if reopened.Status != "open" {
		t.Fatalf("status = %q, want open", reopened.Status)
	}
	if reopened.ClosedAt != nil {
		t.Fatalf("closed_at = %v, want nil", *reopened.ClosedAt)
	}
	if reopened.CloseReason != "" {
		t.Fatalf("close_reason = %q, want empty", reopened.CloseReason)
	}
}

func TestBugIDRoundTrip(t *testing.T) {
	if got := FormatBugID(12); got != "wb12" {
		t.Fatalf("FormatBugID = %q, want wb12", got)
	}
	id, err := ParseBugID("wb12")
	if err != nil {
		t.Fatalf("ParseBugID: %v", err)
	}
	if id != 12 {
		t.Fatalf("ParseBugID = %d, want 12", id)
	}
}

func TestOpenBugDBImportsLegacyJobsDBBugs(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "jobs.db")
	bugPath := filepath.Join(dir, "bugs.db")
	writeLegacyBugDB(t, legacyPath)

	restoreJobsPath := SetDBPath(legacyPath)
	t.Cleanup(restoreJobsPath)
	restoreBugPath := SetBugDBPath(bugPath)
	t.Cleanup(restoreBugPath)

	database, err := OpenBugDB()
	if err != nil {
		t.Fatalf("OpenBugDB: %v", err)
	}
	defer database.Close()

	bugs, err := ListBugs(database, true)
	if err != nil {
		t.Fatalf("ListBugs: %v", err)
	}
	if len(bugs) != 1 {
		t.Fatalf("len(bugs) = %d, want 1", len(bugs))
	}
	if bugs[0].Title != "legacy presentation mismatch" {
		t.Fatalf("title = %q", bugs[0].Title)
	}
	if bugs[0].JobID == nil || *bugs[0].JobID != 2471 {
		t.Fatalf("job id = %v, want 2471", bugs[0].JobID)
	}

	notes, err := ListBugNotes(database, bugs[0].ID)
	if err != nil {
		t.Fatalf("ListBugNotes: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("len(notes) = %d, want legacy note plus import note", len(notes))
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	database, err = OpenBugDB()
	if err != nil {
		t.Fatalf("OpenBugDB second: %v", err)
	}
	defer database.Close()
	notes, err = ListBugNotes(database, bugs[0].ID)
	if err != nil {
		t.Fatalf("ListBugNotes second: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("second import duplicated notes: len(notes) = %d, want 2", len(notes))
	}
}

func writeLegacyBugDB(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE bugs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			status TEXT NOT NULL DEFAULT 'open',
			title TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT 'bug',
			scope TEXT NOT NULL DEFAULT 'infrastructure',
			likelihood TEXT NOT NULL DEFAULT 'unknown',
			severity TEXT NOT NULL DEFAULT 'notice',
			fingerprint TEXT NOT NULL DEFAULT '',
			job_id INTEGER,
			host TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '',
			occurrences INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			closed_at INTEGER,
			close_reason TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE bug_notes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			bug_id INTEGER NOT NULL REFERENCES bugs(id) ON DELETE CASCADE,
			body TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
		INSERT INTO bugs
			(id, status, title, kind, scope, likelihood, severity, fingerprint,
			 job_id, host, summary, detail, occurrences, created_at, updated_at)
		VALUES
			(7, 'open', 'legacy presentation mismatch', 'bug', 'job-specific',
			 'likely', 'warning', 'legacy.presentation', 2471, 'wi3626',
			 'summary', 'detail', 1, 100, 200);
		INSERT INTO bug_notes (bug_id, body, created_at) VALUES (7, 'legacy note', 150);
	`); err != nil {
		t.Fatalf("create legacy bug db: %v", err)
	}
}

// writePreRecurrenceBugDB creates a standalone bugs database with the schema
// as it was before recurrence tracking, so tests can exercise the column
// ensure that runs when a current binary opens it.
func writePreRecurrenceBugDB(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open pre-recurrence bug db: %v", err)
	}
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE bugs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			status TEXT NOT NULL DEFAULT 'open',
			title TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT 'bug',
			scope TEXT NOT NULL DEFAULT 'infrastructure',
			likelihood TEXT NOT NULL DEFAULT 'unknown',
			severity TEXT NOT NULL DEFAULT 'notice',
			fingerprint TEXT NOT NULL DEFAULT '',
			job_id INTEGER,
			host TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '',
			occurrences INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			closed_at INTEGER,
			close_reason TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO bugs
			(id, status, title, fingerprint, occurrences, created_at, updated_at)
		VALUES
			(1, 'open', 'pre-recurrence invariant', 'legacy.closed.fingerprint', 1, 100, 200);
	`); err != nil {
		t.Fatalf("create pre-recurrence bug db: %v", err)
	}
}
