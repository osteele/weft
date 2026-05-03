package terminal

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestAttemptsListHeaderUsesCanonicalJobID(t *testing.T) {
	model := attemptsListModel{
		jobID: 1692,
		job:   &db.Job{ID: 1692, Status: db.StatusQueued, Project: "role-encoding-injection"},
	}

	header := model.renderHeader()
	if !strings.Contains(header, "Attempts for job wj1692") {
		t.Fatalf("header = %q, want canonical job ID", header)
	}
	if strings.Contains(header, "job #1692") {
		t.Fatalf("header = %q, should not use raw numeric job ID", header)
	}
	if strings.Contains(header, "wi1692") {
		t.Fatalf("header = %q, should not use instance ID prefix for jobs", header)
	}
}
