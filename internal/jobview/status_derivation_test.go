package jobview

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// The attempts-authority plan turns on one invariant: the status a job
// presents is derived once and read consistently, not re-derived per surface.
// Three surfaces derive it today and must not drift:
//
//  1. job_status.status — the SQL view, surfaced as job.Status by GetJobByID.
//  2. Job.EffectiveStatus() — the Go overlay used for UI bucketing decisions.
//  3. ExpandJobsForOpenMoves — the list-view expansion that renders move rows.
//
// For a placed job with no open move all three agree. They diverge only in
// two spec-documented cases, each pinned by its own test below:
//   - the unplaced-active override (a hostless job cannot be running), and
//   - the source-authoritative dual-row rendering during an open move
//     (specs/job-move.allium).

func TestStatusDerivation_PlacedJobsAgreeAcrossSurfaces(t *testing.T) {
	database := db.SetupTestDB(t)

	queued := func(t *testing.T) int64 {
		t.Helper()
		id, err := db.RecordQueued(database, "cool30", "/tmp/project", "python train.py", "test")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		return id
	}
	running := func(t *testing.T) int64 {
		t.Helper()
		id := queued(t)
		if _, err := db.CreateAttempt(database, id, "cool30", nil, db.StatusRunning); err != nil {
			t.Fatalf("CreateAttempt running: %v", err)
		}
		return id
	}
	closed := func(t *testing.T, status string, exit *int) int64 {
		t.Helper()
		id := running(t)
		if err := db.CloseAttempt(database, id, status, exit, time.Now().Unix()); err != nil {
			t.Fatalf("CloseAttempt %s: %v", status, err)
		}
		return id
	}

	cases := []struct {
		name       string
		build      func(t *testing.T) int64
		wantStatus string
	}{
		{"queued", queued, db.StatusQueued},
		{"running", running, db.StatusRunning},
		{"completed", func(t *testing.T) int64 { return closed(t, db.StatusCompleted, intPtr(0)) }, db.StatusCompleted},
		{"failed", func(t *testing.T) int64 { return closed(t, db.StatusFailed, intPtr(1)) }, db.StatusFailed},
		{"killed", func(t *testing.T) int64 { return closed(t, db.StatusKilled, nil) }, db.StatusKilled},
		{"canceled", func(t *testing.T) int64 { return closed(t, db.StatusCanceled, nil) }, db.StatusCanceled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobID := tc.build(t)
			job, err := db.GetJobByID(database, jobID)
			if err != nil {
				t.Fatalf("GetJobByID: %v", err)
			}

			// Surface 1: the view drives job.Status.
			if job.Status != tc.wantStatus {
				t.Fatalf("view status = %q, want %q", job.Status, tc.wantStatus)
			}
			// Surface 2: EffectiveStatus agrees for a placed job.
			if got := job.EffectiveStatus(); got != job.Status {
				t.Fatalf("EffectiveStatus %q diverges from view status %q for placed job", got, job.Status)
			}
			// Surface 3: with no open move the expansion is a passthrough —
			// one row, same status.
			ps, err := PlacementStatusForJobs(database, []*db.Job{job}, time.Unix(10_000, 0))
			if err != nil {
				t.Fatalf("PlacementStatusForJobs: %v", err)
			}
			expanded := ExpandJobsForOpenMoves([]*db.Job{job}, ps)
			if len(expanded) != 1 {
				t.Fatalf("expanded rows = %d, want 1 (no open move should not split the row)", len(expanded))
			}
			if expanded[0].Status != job.Status {
				t.Fatalf("expanded status = %q, want view status %q", expanded[0].Status, job.Status)
			}
		})
	}
}

// Documented divergence 1: EffectiveStatus collapses an active status to
// queued when the job has no host, because a hostless job cannot actually be
// running/starting/paused. This is the only sanctioned place where surface 2
// intentionally departs from surface 1.
func TestStatusDerivation_UnplacedActiveStatusCollapsesToQueued(t *testing.T) {
	for _, status := range []string{db.StatusRunning, db.StatusStarting, db.StatusPaused} {
		t.Run(status, func(t *testing.T) {
			job := &db.Job{Status: status}
			if job.TargetKind() != db.JobTargetUnplaced {
				t.Fatalf("precondition: hostless job target = %q, want unplaced", job.TargetKind())
			}
			if got := job.EffectiveStatus(); got != db.StatusQueued {
				t.Fatalf("EffectiveStatus(%q, unplaced) = %q, want %q", status, got, db.StatusQueued)
			}
		})
	}
}

// Documented divergence 2: during an open move the list view renders a
// source-authoritative dim row plus a target row (specs/job-move.allium),
// so ExpandJobsForOpenMoves deliberately produces more than one row from a
// single job_status row. This is the spec-intended difference the matrix
// tolerates.
func TestStatusDerivation_OpenMoveSplitsIntoSourceAndTargetRows(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	sourceLaunchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, sourceLaunchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	sourceAttemptID, err := db.GetAuthoritativeAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetAuthoritativeAttemptID: %v", err)
	}
	targetLaunchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "runpod"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	intent, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:           jobID,
		SourceAttemptID: &sourceAttemptID,
		SourceLaunchID:  &sourceLaunchID,
		TargetKind:      db.MoveTargetNew,
		TargetLaunchID:  &targetLaunchID,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if _, err := db.CreateMoveTargetAttempt(database, intent.ID, jobID, "", &targetLaunchID, db.StatusQueued); err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	ps, err := PlacementStatusForJobs(database, []*db.Job{job}, time.Unix(10_000, 0))
	if err != nil {
		t.Fatalf("PlacementStatusForJobs: %v", err)
	}
	expanded := ExpandJobsForOpenMoves([]*db.Job{job}, ps)
	if len(expanded) != 2 {
		t.Fatalf("expanded rows = %d, want 2 (source + target) during open move", len(expanded))
	}
	source, target := expanded[0], expanded[1]
	if !source.DisplayMoveDim {
		t.Fatalf("first row should be the dim source row: %+v", source)
	}
	if target.DisplayMoveDim {
		t.Fatalf("second row should be the full-contrast target row: %+v", target)
	}
}

func intPtr(v int) *int { return &v }
