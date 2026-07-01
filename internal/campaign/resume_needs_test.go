package campaign

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

type fakeResumeLister map[string][]r2.ObjectInfo

func (f fakeResumeLister) ListObjects(_ context.Context, prefix string) ([]r2.ObjectInfo, error) {
	return append([]r2.ObjectInfo(nil), f[prefix]...), nil
}

func TestListAttemptResumeCloudNeeds(t *testing.T) {
	jobID := int64(3739)
	runID := int64(35764)
	outputsPrefix := r2keys.JobAttemptOutputsPrefix(jobID, runID)
	artifactsPrefix := r2keys.JobAttemptArtifactFilesPrefix(jobID, runID)
	lister := fakeResumeLister{
		outputsPrefix: {
			{Key: outputsPrefix + "output/exp/ckpt_latest.pt"},
			{Key: outputsPrefix + "output/exp/metrics.json"},
		},
		artifactsPrefix: {
			{Key: artifactsPrefix + "output/exp/ckpt_latest.pt"},
			{Key: artifactsPrefix + "logs/train.log"},
		},
	}

	got, err := listAttemptResumeCloudNeeds(context.Background(), lister, jobID, runID)
	if err != nil {
		t.Fatalf("listAttemptResumeCloudNeeds: %v", err)
	}
	want := []cloud.CloudNeed{
		{Spec: "logs/train.log:3739", Path: "logs/train.log", R2Key: artifactsPrefix + "logs/train.log"},
		{Spec: "output/exp/ckpt_latest.pt:3739", Path: "output/exp/ckpt_latest.pt", R2Key: outputsPrefix + "output/exp/ckpt_latest.pt"},
		{Spec: "output/exp/metrics.json:3739", Path: "output/exp/metrics.json", R2Key: outputsPrefix + "output/exp/metrics.json"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resume needs = %#v, want %#v", got, want)
	}
}

func TestAppendResumeCloudNeedsSkipsDuplicatePaths(t *testing.T) {
	prev := resolveResumeCloudNeedsFunc
	t.Cleanup(func() { resolveResumeCloudNeedsFunc = prev })
	resolveResumeCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ *db.Job) ([]cloud.CloudNeed, error) {
		return []cloud.CloudNeed{
			{Spec: "output/model.pt:42", Path: "output/model.pt", R2Key: "resume/model"},
			{Spec: "output/state.json:42", Path: "output/state.json", R2Key: "resume/state"},
		}, nil
	}

	existing := []cloud.CloudNeed{{Spec: "output/model.pt:99", Path: "output/model.pt", R2Key: "explicit/model"}}
	got, restaged, err := appendResumeCloudNeeds(context.Background(), nil, nil, &db.Job{ID: 42}, existing)
	if err != nil {
		t.Fatalf("appendResumeCloudNeeds: %v", err)
	}
	if !restaged {
		t.Fatal("restaged = false, want true")
	}
	want := []cloud.CloudNeed{
		{Spec: "output/model.pt:99", Path: "output/model.pt", R2Key: "explicit/model"},
		{Spec: "output/state.json:42", Path: "output/state.json", R2Key: "resume/state"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged needs = %#v, want %#v", got, want)
	}
}

func TestPredecessorAttemptID(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "train", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	firstLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusFailed})
	if err != nil {
		t.Fatalf("CreateLaunch first: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, firstLaunch); err != nil {
		t.Fatalf("SetJobLaunchID first: %v", err)
	}
	firstAttempt, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID first: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, end_time = ?, cloud_outcome = ? WHERE id = ?`,
		db.StatusCanceled, int64(100), db.AttemptOutcomePreempted, firstAttempt,
	); err != nil {
		t.Fatalf("close first attempt: %v", err)
	}
	secondLaunch, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch second: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, secondLaunch); err != nil {
		t.Fatalf("SetJobLaunchID second: %v", err)
	}
	secondAttempt, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID second: %v", err)
	}

	got, ok, err := predecessorAttemptID(database, jobID, secondAttempt)
	if err != nil {
		t.Fatalf("predecessorAttemptID: %v", err)
	}
	if !ok || got != firstAttempt {
		t.Fatalf("predecessor = (%d, %v), want (%d, true)", got, ok, firstAttempt)
	}
}
