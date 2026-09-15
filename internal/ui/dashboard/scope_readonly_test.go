package dashboard

import (
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
)

func TestFilterJobsByScopeUsesExactStoredAttributionAcrossStates(t *testing.T) {
	database := db.SetupTestDB(t)
	const (
		session = "session-a"
		project = "/projects/weft"
	)

	record := func(name, owner, root, status string, processed bool) int64 {
		t.Helper()
		jobID, err := db.RecordQueued(database, "studio", root, "echo "+name, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetJobSubmitterSession(database, jobID, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`UPDATE jobs SET project_root = ? WHERE id = ?`, root, jobID); err != nil {
			t.Fatal(err)
		}
		if processed {
			if err := db.SetJobTags(database, jobID, []string{"processed"}); err != nil {
				t.Fatal(err)
			}
		}
		switch status {
		case db.StatusQueued:
		case db.StatusRunning:
			if err := db.UpdateAttemptRunning(database, jobID); err != nil {
				t.Fatal(err)
			}
		default:
			exitCode := 0
			if status == db.StatusFailed {
				exitCode = 1
			}
			if err := db.CloseAttempt(database, jobID, status, &exitCode, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
		}
		return jobID
	}

	want := map[int64]bool{
		record("queued", session, project, db.StatusQueued, false):       true,
		record("running", session, project, db.StatusRunning, false):     true,
		record("completed", session, project, db.StatusCompleted, false): true,
		record("processed", session, project, db.StatusFailed, true):     true,
		record("killed", session, project, db.StatusKilled, false):       true,
	}
	record("other-session", "session-b", project, db.StatusRunning, false)
	record("other-project", session, "/projects/other", db.StatusCompleted, false)
	record("unattributed", "", project, db.StatusQueued, false)

	jobs, err := db.ListJobs(database, "", "", 1000, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := FilterJobsByScope(database, jobs, &JobScope{SubmitterSession: session, ProjectRoot: project})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != len(want) {
		t.Fatalf("scoped job count = %d, want %d: %+v", len(scoped), len(want), scoped)
	}
	for _, job := range scoped {
		if !want[job.ID] {
			t.Fatalf("unexpected scoped job %d (%s)", job.ID, job.Status)
		}
	}

	model := NewModelWithOptions(database, ModelOptions{
		InitialSnapshot: &InitialSnapshot{
			Jobs:  scoped,
			Hosts: []*Host{{Name: "studio", Status: HostStatusChecking}},
		},
		JobScope: &JobScope{SubmitterSession: session, ProjectRoot: project},
		ReadOnly: false,
	})
	t.Cleanup(model.cancel)
	if model.syncWorker != nil || model.cloudDiscoveryFn != nil || model.llmInitFn != nil {
		t.Fatal("read-only model initialized a mutating background integration")
	}
	if model.jobFilter != jobFilterAll || model.jobHostFilterMode != hostFilterAll || len(model.jobs) != len(want) {
		t.Fatalf("scoped startup filters = (%v, %v), jobs = %d; want all %d", model.jobFilter, model.jobHostFilterMode, len(model.jobs), len(want))
	}
	if !model.readOnly {
		t.Fatal("job scope did not imply read-only model behavior")
	}
	if len(model.hosts) != 1 || model.hosts[0].Status != HostStatusUnknown {
		t.Fatalf("read-only host status = %+v, want cached unknown status", model.hosts)
	}

	unknown, err := FilterJobsByScope(database, jobs, &JobScope{SubmitterSession: "missing", ProjectRoot: project})
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 0 {
		t.Fatalf("unknown session fell back to %d jobs", len(unknown))
	}
}

func TestScopedJobQueryAppliesAttributionBeforeLimit(t *testing.T) {
	database := db.SetupTestDB(t)
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := db.RecordQueuedWithGPUTx(tx, "studio", "/projects/weft", "echo target", "target", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(
		`UPDATE jobs SET submitter_session = ?, project_root = ? WHERE id = ?`,
		"session-a", "/projects/weft", targetID,
	); err != nil {
		t.Fatal(err)
	}
	for range 1001 {
		if _, err := db.RecordQueuedWithGPUTx(tx, "studio", "/projects/other", "echo unrelated", "unrelated", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	scope := &JobScope{SubmitterSession: "session-a", ProjectRoot: "/projects/weft"}
	snapshot, err := LoadInitialSnapshotForScope(database, DefaultHostCacheDuration, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].ID != targetID {
		t.Fatalf("scoped snapshot jobs = %+v, want older job %d", snapshot.Jobs, targetID)
	}

	model := Model{database: database, jobScope: scope}
	msg, ok := model.refreshJobs()().(jobsRefreshedMsg)
	if !ok {
		t.Fatalf("scoped refresh message has unexpected type")
	}
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if len(msg.jobs) != 1 || msg.jobs[0].ID != targetID {
		t.Fatalf("scoped refreshed jobs = %+v, want older job %d", msg.jobs, targetID)
	}
}

func TestReadOnlyDashboardRejectsEveryMutationKey(t *testing.T) {
	job := &db.Job{ID: 42, Status: db.StatusQueued}
	for _, keyName := range []string{"E", "k", "p", "d", "R", "y", "x", "n", "P", "S", "g", "F", "e", "G", "D", "c"} {
		t.Run(keyName, func(t *testing.T) {
			model := Model{
				readOnly:           true,
				viewMode:           ViewModeJobs,
				jobs:               []*db.Job{job},
				selectedJob:        job,
				jobSelectionActive: true,
			}
			updated, _ := model.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(keyName)})
			got := updated.(Model)
			if !got.flash.IsError || got.flash.Message != "Read-only mode: action disabled" {
				t.Fatalf("key %q flash = %+v", keyName, got.flash)
			}
			if got.inputMode || got.editMode || got.showCloudMenu || got.restarting || got.creatingJob {
				t.Fatalf("key %q entered a mutating mode", keyName)
			}
		})
	}

	inputs := make([]textinput.Model, 7)
	for i := range inputs {
		inputs[i] = textinput.New()
	}
	interactive := Model{viewMode: ViewModeJobs, inputs: inputs}
	updated, _ := interactive.handleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if !updated.(Model).inputMode {
		t.Fatal("ordinary interactive dashboard did not preserve new-job action")
	}
}

func TestReadOnlyLogTickSchedulesOrdinaryJobRefresh(t *testing.T) {
	job := &db.Job{
		ID:      42,
		Host:    "studio",
		Status:  db.StatusRunning,
		Backend: db.BackendQueueRunner,
	}
	model := Model{
		readOnly:           true,
		selectedJob:        job,
		detailTab:          DetailTabLogs,
		logRefreshInterval: time.Minute,
	}

	_, cmd := model.Update(logTickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("read-only log tick scheduled no work")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("read-only log tick message = %T, want tea.BatchMsg", cmd())
	}
	if len(batch) != 2 {
		t.Fatalf("read-only log tick scheduled %d commands, want ticker and direct log fetch", len(batch))
	}
}
