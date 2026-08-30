package queueblock

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestJobDependenciesSatisfied(t *testing.T) {
	database := db.SetupTestDB(t)

	upstreamSucceeded, err := db.RecordJobStarting(database, "cool30", "/tmp/p", "cmd", "ok")
	if err != nil {
		t.Fatalf("RecordJobStarting ok: %v", err)
	}
	if err := db.RecordCompletionByID(database, upstreamSucceeded, 0, time.Now().Unix()); err != nil {
		t.Fatalf("RecordCompletionByID ok: %v", err)
	}

	upstreamFailed, err := db.RecordJobStarting(database, "cool30", "/tmp/p", "cmd", "fail")
	if err != nil {
		t.Fatalf("RecordJobStarting fail: %v", err)
	}
	if err := db.RecordCompletionByID(database, upstreamFailed, 1, time.Now().Unix()); err != nil {
		t.Fatalf("RecordCompletionByID fail: %v", err)
	}

	upstreamRunning, err := db.RecordJobStarting(database, "cool30", "/tmp/p", "cmd", "run")
	if err != nil {
		t.Fatalf("RecordJobStarting run: %v", err)
	}

	tests := []struct {
		name        string
		spec        string
		wantOK      bool
		wantSnippet string
	}{
		{name: "empty", spec: "", wantOK: true},
		{name: "success dependency satisfied", spec: strconv.FormatInt(upstreamSucceeded, 10), wantOK: true},
		{name: "success dependency waits on running", spec: strconv.FormatInt(upstreamRunning, 10), wantOK: false, wantSnippet: "to succeed"},
		{name: "success dependency rejects failed", spec: strconv.FormatInt(upstreamFailed, 10), wantOK: false, wantSnippet: "failed"},
		{name: "any dependency accepts failed", spec: strconv.FormatInt(upstreamFailed, 10) + ":any", wantOK: true},
		{name: "any dependency waits on running", spec: strconv.FormatInt(upstreamRunning, 10) + ":any", wantOK: false, wantSnippet: "to finish"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, ok := JobDependenciesSatisfied(database, ParseJobDependencies(tt.spec))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (reason %q)", ok, tt.wantOK, reason)
			}
			if tt.wantSnippet != "" && !strings.Contains(reason, tt.wantSnippet) {
				t.Fatalf("reason = %q, want substring %q", reason, tt.wantSnippet)
			}
		})
	}
}

func TestHydrateWaitingOnJobDependencyReasons(t *testing.T) {
	database := db.SetupTestDB(t)
	upstreamID, err := db.RecordJobStarting(database, "cool30", "/tmp/p", "cmd", "producer")
	if err != nil {
		t.Fatalf("RecordJobStarting: %v", err)
	}
	job := &db.Job{
		ID:      200,
		Status:  db.StatusQueued,
		DepSpec: strconv.FormatInt(upstreamID, 10),
	}

	HydrateWaitingOnJobDependencyReasons(database, []*db.Job{job})

	if !strings.Contains(job.QueueBlockedReason, "to succeed") {
		t.Fatalf("QueueBlockedReason = %q, want dependency wait", job.QueueBlockedReason)
	}
}

func TestPropagateTerminalDependencySkipsStrictDescendants(t *testing.T) {
	database := db.SetupTestDB(t)
	parentID, err := db.RecordJobStarting(database, "cool30", "/tmp/p", "false", "canary")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordCompletionByID(database, parentID, 1, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	strictID, err := db.RecordQueued(database, "", "/tmp/p", "echo strict", "strict")
	if err != nil {
		t.Fatal(err)
	}
	anyID, err := db.RecordQueued(database, "", "/tmp/p", "echo any", "any")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE jobs SET dep_spec = CASE id WHEN ? THEN ? WHEN ? THEN ? END WHERE id IN (?, ?)`,
		strictID, strconv.FormatInt(parentID, 10), anyID, strconv.FormatInt(parentID, 10)+":any", strictID, anyID); err != nil {
		t.Fatal(err)
	}

	skipped, err := PropagateTerminalDependencySkips(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != strictID {
		t.Fatalf("skipped = %v, want [%d]", skipped, strictID)
	}
	strict, _ := db.GetJobByID(database, strictID)
	if strict.EffectiveStatus() != db.StatusSkipped || strict.FailureReason != db.FailureReasonDependencyFailed {
		t.Fatalf("strict = status %q failure %q", strict.EffectiveStatus(), strict.FailureReason)
	}
	any, _ := db.GetJobByID(database, anyID)
	if any.EffectiveStatus() != db.StatusQueued {
		t.Fatalf("after-any status = %q, want queued", any.EffectiveStatus())
	}
}
