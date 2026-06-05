package db

import (
	"database/sql"
	"path/filepath"
	"testing"
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
