package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestRunSetsJobEnvVars(t *testing.T) {
	out := filepath.Join(t.TempDir(), "captured")
	job := &db.Job{
		ID:          123,
		Host:        "cool30",
		WorkingDir:  "/home/user/proj",
		Description: "test sweep",
	}
	exit := 0
	Run(`printf '%s|%s|%s|%s|%s' "$WEFT_JOB_ID" "$WEFT_JOB_STATUS" "$WEFT_JOB_EXIT_CODE" "$WEFT_JOB_DIR" "$WEFT_JOB_SUMMARY" > `+out,
		job, db.StatusCompleted, &exit)

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify command did not run: %v", err)
	}
	got := string(data)
	for _, want := range []string{"123", db.StatusCompleted, "|0|", "/home/user/proj", "test sweep"} {
		if !strings.Contains(got, want) {
			t.Errorf("captured env %q missing %q", got, want)
		}
	}
}

func TestRunFailureExitCodeInSummary(t *testing.T) {
	out := filepath.Join(t.TempDir(), "captured")
	job := &db.Job{ID: 7, Host: "h", Description: "d"}
	exit := 2
	Run(`printf '%s' "$WEFT_JOB_SUMMARY" > `+out, job, db.StatusFailed, &exit)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("notify command did not run: %v", err)
	}
	if !strings.Contains(string(data), "exit 2") {
		t.Errorf("summary %q missing exit code", string(data))
	}
}

func TestRunCommandFailureIsNonFatal(t *testing.T) {
	job := &db.Job{ID: 1}
	Run("exit 3", job, db.StatusFailed, nil) // must not panic or exit
}
