package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
