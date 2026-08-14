package terminal

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestListTUIDiagnoseKeyRunsForSelectedJob(t *testing.T) {
	m := listTUIModel{
		jobs: []*db.Job{{ID: 6255, Status: db.StatusQueued}},
	}
	next, cmd, handled := handleListKeyBinding(m, "z", listFlatKeyBindings())
	if !handled || cmd == nil {
		t.Fatalf("z handled=%v cmd=%v, want diagnose command", handled, cmd)
	}
	got := next.(listTUIModel)
	if !strings.Contains(got.statusMessage, "Diagnosing job #6255") {
		t.Fatalf("status = %q, want selected diagnosis", got.statusMessage)
	}
}

func TestListTUIDiagnoseKeyRequiresJobRow(t *testing.T) {
	m := listTUIModel{}
	next, cmd, handled := handleListKeyBinding(m, "z", listFlatKeyBindings())
	if !handled || cmd != nil {
		t.Fatalf("z handled=%v cmd=%v, want handled without command", handled, cmd)
	}
	got := next.(listTUIModel)
	if got.statusMessage != "Select a job row to diagnose" {
		t.Fatalf("status = %q", got.statusMessage)
	}
}
