package notify

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

// writeNotifyConfig points config.Load at a throwaway config whose notify
// command captures WEFT_JOB_SUBMITTER_SESSION, and returns the capture path.
func writeNotifyConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "captured")
	tomlPath := filepath.Join(dir, "config.toml")
	body := "[notifications]\n  command = \"printf '[%s]' \\\"$WEFT_JOB_SUBMITTER_SESSION\\\" > " + out + "\"\n"
	if err := os.WriteFile(tomlPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Cleanup(config.SetConfigPathsForTesting(tomlPath, ""))
	return out
}

// The whole point of the feature: the session recorded at submit time reaches
// the notify command, so it can address one session instead of broadcasting.
func TestJobTerminalPassesRecordedSubmitterSession(t *testing.T) {
	out := writeNotifyConfig(t)
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp/project", "echo hi", "")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobSubmitterSession(database, jobID, "claude-xyz-789"); err != nil {
		t.Fatalf("SetJobSubmitterSession: %v", err)
	}

	exit := 0
	JobTerminal(database, jobID, db.StatusCompleted, &exit)

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify command did not run: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "[claude-xyz-789]" {
		t.Errorf("notify saw submitter session %q, want %q", got, "[claude-xyz-789]")
	}
}

func TestJobTerminalWithoutSubmitterSessionStillNotifies(t *testing.T) {
	out := writeNotifyConfig(t)
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp/project", "echo hi", "")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	exit := 0
	JobTerminal(database, jobID, db.StatusCompleted, &exit)

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify command did not run for an unattributed job: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "[]" {
		t.Errorf("notify saw submitter session %q, want empty", got)
	}
}

func TestJobTerminalSeparatesProjectScopedSessionNoteFromSummary(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "notification")
	tomlPath := filepath.Join(dir, "config.toml")
	body := "[notifications]\n  command = \"printf '%s\\n---\\n%s' \\\"$WEFT_JOB_SUMMARY\\\" \\\"$WEFT_JOB_SESSION_NOTE\\\" > " + out + "\"\n"
	if err := os.WriteFile(tomlPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.SetConfigPathsForTesting(tomlPath, ""))
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp/project", "echo hi", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobSubmitterSession(database, jobID, "session-notify"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobProject(database, jobID, "project-a"); err != nil {
		t.Fatal(err)
	}
	exit := 0
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exit, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	otherID, err := db.RecordQueued(database, "test-host", "/tmp/other", "echo other", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobSubmitterSession(database, otherID, "session-notify"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobProject(database, otherID, "project-b"); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseAttempt(database, otherID, db.StatusCompleted, &exit, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	JobTerminal(database, jobID, db.StatusCompleted, &exit)

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify command did not run: %v", err)
	}
	parts := strings.SplitN(string(data), "\n---\n", 2)
	if len(parts) != 2 {
		t.Fatalf("notification variables were not split: %q", data)
	}
	summary, note := parts[0], parts[1]
	if summary != "Job wj"+strconv.FormatInt(jobID, 10)+" completed" {
		t.Fatalf("WEFT_JOB_SUMMARY = %q", summary)
	}
	if strings.Contains(summary, "This session") || !strings.Contains(note, "This session has 1 unprocessed terminal job") || !strings.Contains(note, "process-results skill") {
		t.Fatalf("summary/note = %q / %q", summary, note)
	}
	if strings.Contains(note, "2 unprocessed") {
		t.Fatalf("session note crossed project boundary: %q", note)
	}
}
