package terminal

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/jobview"
)

var updateListGoldens = flag.Bool("update-list-goldens", false, "update list render golden files")

func TestListRenderGoldenFrames(t *testing.T) {
	oldProfile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(oldProfile) })

	jobs, placement := listGoldenFixture()
	now := time.Unix(20_000, 0)

	grouped := renderJobListGroupedStatusPlainWithOptions(jobs, 96, groupedStatusRenderOptions{
		placementStatusByJob: placement,
		launchLiveByID: map[int64]*db.LaunchLiveState{
			7001: {LaunchID: 7001, BootstrapStage: "deps_installing"},
		},
		launchByID: map[int64]*db.Launch{
			7001: {ID: 7001, Status: db.LaunchStatusLaunching, CreatedAt: now.Add(-4 * time.Minute).Unix()},
		},
		now: now,
	})
	assertGoldenFrame(t, "grouped_status", grouped)

	flatJobs := jobview.ExpandJobsForOpenMoves(jobs, placement)
	flat := renderJobListPlain(flatJobs, 120)
	assertGoldenFrame(t, "flat_list", flat)
}

func listGoldenDiff(want, got string) string {
	wantLines := strings.Split(strings.TrimRight(want, "\n"), "\n")
	gotLines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	var b strings.Builder
	maxLen := max(len(wantLines), len(gotLines))
	for i := 0; i < maxLen; i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w == g {
			continue
		}
		if i < len(wantLines) {
			b.WriteString("- " + w + "\n")
		}
		if i < len(gotLines) {
			b.WriteString("+ " + g + "\n")
		}
	}
	return b.String()
}

func listGoldenFixture() ([]*db.Job, map[int64]jobview.PlacementStatus) {
	sourceLaunchID := int64(6201)
	targetLaunchID := int64(6202)
	launchingID := int64(7001)
	sourceAttemptID := int64(31)
	targetAttemptID := int64(32)
	endOK := int64(19_500)
	endFailed := int64(19_100)
	exitOK := 0
	exitFailed := 2

	jobs := []*db.Job{
		{
			ID:          501,
			Status:      db.StatusRunning,
			LaunchID:    &sourceLaunchID,
			LatestRunID: &sourceAttemptID,
			Project:     "alpha",
			Description: "open move source and target",
			StartTime:   18_000,
		},
		{
			ID:                 502,
			Status:             db.StatusQueued,
			Project:            "alpha",
			Description:        "blocked rental offer",
			QueueBlockedReason: "planner: offer fetch unavailable: cached offer snapshot missing",
			CreatedAt:          18_200,
		},
		{
			ID:          503,
			Status:      db.StatusPendingPlacement,
			LaunchID:    &launchingID,
			Project:     "beta",
			Description: "launching setup",
			CreatedAt:   18_400,
		},
		{
			ID:          504,
			Status:      db.StatusCompleted,
			Host:        "cool30",
			Project:     "beta",
			Description: "finished cleanly",
			EndTime:     &endOK,
			ExitCode:    &exitOK,
		},
		{
			ID:          505,
			Status:      db.StatusCompleted,
			Host:        "cool30",
			Project:     "gamma",
			Description: "finished with nonzero exit",
			EndTime:     &endFailed,
			ExitCode:    &exitFailed,
			RetryCount:  1,
		},
		{
			ID:          506,
			Status:      db.StatusCanceled,
			Project:     "gamma",
			Description: "operator canceled",
			CreatedAt:   18_800,
		},
	}
	placement := map[int64]jobview.PlacementStatus{
		501: {
			JobID:  501,
			Bucket: jobview.BucketRunning,
			Move: &jobview.MoveDisplay{
				IntentID:        44,
				State:           db.MoveIntentStateOpen,
				SourceAttemptID: &sourceAttemptID,
				TargetAttemptID: &targetAttemptID,
				SourceLabel:     "wi6201",
				TargetLabel:     "wi6202",
				Phase:           "waiting for destination acceptance",
				AttemptsByID: map[int64]db.JobAttempt{
					sourceAttemptID: {ID: sourceAttemptID, JobID: 501, AttemptNumber: 1, LaunchID: &sourceLaunchID, Status: db.StatusRunning, StartTime: testInt64Ptr(18_000)},
					targetAttemptID: {ID: targetAttemptID, JobID: 501, AttemptNumber: 2, LaunchID: &targetLaunchID, Status: db.StatusQueued, QueuedAt: testInt64Ptr(18_500)},
				},
			},
		},
	}
	return jobs, placement
}

func assertGoldenFrame(t *testing.T, name, got string) {
	t.Helper()
	ansiPath := filepath.Join("testdata", "list_golden", name+".ansi")
	textPath := filepath.Join("testdata", "list_golden", name+".txt")
	if *updateListGoldens {
		if err := os.MkdirAll(filepath.Dir(ansiPath), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(ansiPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write %s: %v", ansiPath, err)
		}
		if err := os.WriteFile(textPath, []byte(ansi.Strip(got)), 0o644); err != nil {
			t.Fatalf("write %s: %v", textPath, err)
		}
		return
	}
	wantANSI, err := os.ReadFile(ansiPath)
	if err != nil {
		t.Fatalf("read %s (regenerate with -update-list-goldens): %v", ansiPath, err)
	}
	if got != string(wantANSI) {
		t.Fatalf("%s ANSI golden mismatch.\n\ndiff:\n%s", name, listGoldenDiff(string(wantANSI), got))
	}
	wantText, err := os.ReadFile(textPath)
	if err != nil {
		t.Fatalf("read %s (regenerate with -update-list-goldens): %v", textPath, err)
	}
	if stripped := ansi.Strip(got); stripped != string(wantText) {
		t.Fatalf("%s stripped golden mismatch.\n\ndiff:\n%s", name, listGoldenDiff(string(wantText), stripped))
	}
}
