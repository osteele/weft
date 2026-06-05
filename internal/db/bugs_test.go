package db

import (
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
