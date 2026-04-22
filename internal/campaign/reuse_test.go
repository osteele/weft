package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/r2"
)

func TestMatchJobToInstance_GPUClass(t *testing.T) {
	tests := []struct {
		name         string
		jobClass     string
		instClass    string
		instResolved string
		want         bool
	}{
		{"exact match", "A100", "A100", "", true},
		{"case insensitive", "a100", "A100", "", true},
		{"mismatch", "A100", "RTX_4090", "", false},
		{"empty job class matches anything", "", "A100", "", true},
		{"match against resolved name", "A100", "RTX_4090", "A100", true},
		{"a6000 alias matches RTX_A6000", "a6000", "RTX_A6000", "", true},
		{"rtx-5090 alias matches RTX_5090", "rtx-5090", "RTX_5090", "", true},
		{"rtx-4080 alias matches RTX 4080S resolved", "rtx-4080", "RTX_4080S", "RTX 4080S", true},
		{"volta matches Tesla V100 resolved", "volta", "nvidia", "Tesla V100", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &db.Job{GPUClass: tt.jobClass}
			cap := InstanceCapacity{
				Instance: &db.Launch{
					GPUClass:        tt.instClass,
					ResolvedGPUName: tt.instResolved,
					GPUMemGB:        80,
				},
				DiskFreeGB: 100,
			}
			got, reason := MatchJobToInstance(job, cap)
			if got != tt.want {
				t.Errorf("MatchJobToInstance() = %v, want %v (reason: %s)", got, tt.want, reason)
			}
		})
	}
}

func TestMatchJobToInstance_GPUMemory(t *testing.T) {
	mem24 := 24
	mem80 := 80

	tests := []struct {
		name      string
		jobMemGB  *int
		instMemGB int
		want      bool
	}{
		{"no job mem requirement", nil, 24, true},
		{"exact match", &mem24, 24, true},
		{"sufficient", &mem24, 80, true},
		{"insufficient", &mem80, 24, false},
		// Regression: a running rental with unknown per-GPU memory (0 in DB)
		// is rejected by the matcher. The fix is at launch time — Instance
		// rows must record offer.GPUMemGB so this check has real data.
		{"unknown instance memory rejected", &mem24, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &db.Job{GPUMemGB: tt.jobMemGB}
			cap := InstanceCapacity{
				Instance:   &db.Launch{GPUMemGB: tt.instMemGB},
				DiskFreeGB: 100,
			}
			got, reason := MatchJobToInstance(job, cap)
			if got != tt.want {
				t.Errorf("MatchJobToInstance() = %v, want %v (reason: %s)", got, tt.want, reason)
			}
		})
	}
}

func TestMatchJobToInstance_DiskCompatibility(t *testing.T) {
	tests := []struct {
		name              string
		jobInputs         []string
		provisionedInputs []string
		diskFreeGB        int
		instDiskGB        int
		want              bool
	}{
		{"no inputs needed", nil, nil, 50, 100, true},
		{"all inputs already cached", []string{"hf:model-a"}, []string{"hf:model-a"}, 0, 100, true},
		{"no disk info skips check", []string{"hf:model-a"}, nil, 0, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &db.Job{Inputs: tt.jobInputs}
			cap := InstanceCapacity{
				Instance:          &db.Launch{DiskGB: tt.instDiskGB},
				ProvisionedInputs: tt.provisionedInputs,
				DiskFreeGB:        tt.diskFreeGB,
			}
			got, _ := MatchJobToInstance(job, cap)
			if got != tt.want {
				t.Errorf("MatchJobToInstance() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchJobToInstance_GraceDeadline(t *testing.T) {
	job := &db.Job{}

	// Grace with enough time remaining
	cap := InstanceCapacity{
		Instance:       &db.Launch{Status: db.LaunchStatusGrace},
		GraceRemaining: 10 * time.Minute,
		DiskFreeGB:     50,
	}
	got, _ := MatchJobToInstance(job, cap)
	if !got {
		t.Error("expected compatible with 10m grace remaining")
	}

	// Grace with too little time
	cap.GraceRemaining = 2 * time.Minute
	got, _ = MatchJobToInstance(job, cap)
	if got {
		t.Error("expected incompatible with 2m grace remaining")
	}

	// Running instance (no grace concern)
	cap.GraceRemaining = 0
	got, _ = MatchJobToInstance(job, cap)
	if !got {
		t.Error("expected compatible with running instance")
	}
}

func TestRankForJob(t *testing.T) {
	job := &db.Job{
		Inputs: []string{"hf:model-a", "hf:model-b"},
	}

	graceInstance := InstanceCapacity{
		Instance: &db.Launch{
			ID:       1,
			Status:   db.LaunchStatusGrace,
			GPUMemGB: 80,
		},
		GraceRemaining:    10 * time.Minute,
		ProvisionedInputs: []string{"hf:model-a"},
		DiskFreeGB:        100,
	}

	runningInstance := InstanceCapacity{
		Instance: &db.Launch{
			ID:       2,
			Status:   db.LaunchStatusRunning,
			GPUMemGB: 80,
		},
		ProvisionedInputs: []string{"hf:model-a", "hf:model-b"},
		DiskFreeGB:        100,
	}

	ranked := RankForJob(job, []InstanceCapacity{runningInstance, graceInstance})

	if len(ranked) != 2 {
		t.Fatalf("expected 2 results, got %d", len(ranked))
	}

	// Grace should come first (free)
	if ranked[0].Instance.ID != 1 {
		t.Errorf("expected grace instance first, got instance %d", ranked[0].Instance.ID)
	}
}

func TestSubtractInputs(t *testing.T) {
	tests := []struct {
		name        string
		jobInputs   []string
		provisioned []string
		want        int
	}{
		{"all new", []string{"a", "b"}, nil, 2},
		{"all cached", []string{"a", "b"}, []string{"a", "b"}, 0},
		{"partial overlap", []string{"a", "b", "c"}, []string{"a"}, 2},
		{"empty job", nil, []string{"a"}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := subtractInputs(tt.jobInputs, tt.provisioned)
			if len(result) != tt.want {
				t.Errorf("subtractInputs() returned %d items, want %d", len(result), tt.want)
			}
		})
	}
}

func TestFindReusableInstancesExcludesActiveTerminationIntent(t *testing.T) {
	database := db.SetupTestDB(t)

	runningID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(running): %v", err)
	}
	terminatingID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(terminating): %v", err)
	}
	if err := db.UpdateLaunchTerminationIntent(database, terminatingID, &instanceintent.Marker{
		TerminalStatus:       db.LaunchStatusCompleted,
		TerminationReason:    db.TerminationReasonCompleted,
		RequestedAtUnix:      time.Now().Add(-30 * time.Second).Unix(),
		DestroyStartedAtUnix: time.Now().Add(-20 * time.Second).Unix(),
	}); err != nil {
		t.Fatalf("UpdateLaunchTerminationIntent: %v", err)
	}

	instances, err := FindReusableInstances(database)
	if err != nil {
		t.Fatalf("FindReusableInstances: %v", err)
	}

	if len(instances) != 1 {
		t.Fatalf("expected 1 reusable instance, got %d", len(instances))
	}
	if instances[0].Instance.ID != runningID {
		t.Fatalf("reusable instance id = %d, want %d", instances[0].Instance.ID, runningID)
	}
}

func TestFindReusableInstancesIncludesNormalGraceAndRunningInstances(t *testing.T) {
	database := db.SetupTestDB(t)

	runningID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(running): %v", err)
	}
	graceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusGrace,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(grace): %v", err)
	}
	if err := db.SetLaunchGraceStarted(database, graceID, time.Now().Add(10*time.Minute).Unix()); err != nil {
		t.Fatalf("SetLaunchGraceStarted: %v", err)
	}

	instances, err := FindReusableInstances(database)
	if err != nil {
		t.Fatalf("FindReusableInstances: %v", err)
	}

	if len(instances) != 2 {
		t.Fatalf("expected 2 reusable instances, got %d", len(instances))
	}
	got := map[int64]bool{}
	for _, inst := range instances {
		got[inst.Instance.ID] = true
	}
	if !got[runningID] || !got[graceID] {
		t.Fatalf("reusable instances = %v, want both running=%d and grace=%d", got, runningID, graceID)
	}
}

func TestPlanReuse_JobIDOrder(t *testing.T) {
	// Jobs with different GPU classes should still be assigned in ID order.
	instances := []InstanceCapacity{{
		Instance: &db.Launch{
			ID:       1,
			Status:   db.LaunchStatusGrace,
			GPUMemGB: 80,
		},
		GraceRemaining: 30 * time.Minute,
		DiskFreeGB:     200,
	}}

	// Submit jobs out of ID order (higher ID first), no GPU class constraint
	jobs := []*db.Job{
		{ID: 408, Status: "queued"},
		{ID: 397, Status: "queued"},
	}

	assignments, remaining := PlanReuse(jobs, instances)
	if len(remaining) != 0 {
		t.Fatalf("expected 0 remaining, got %d", len(remaining))
	}
	if len(assignments) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(assignments))
	}

	// Assignments should preserve input order (caller is responsible for sorting)
	if assignments[0].Job.ID != 408 || assignments[1].Job.ID != 397 {
		t.Errorf("PlanReuse changed job order: got [%d, %d], want [408, 397]",
			assignments[0].Job.ID, assignments[1].Job.ID)
	}
}

func TestSubmitJobsToInstanceRejectsActiveTerminationIntent(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.UpdateLaunchTerminationIntent(database, instanceID, &instanceintent.Marker{
		TerminalStatus:       db.LaunchStatusCompleted,
		TerminationReason:    db.TerminationReasonCompleted,
		RequestedAtUnix:      time.Now().Add(-35 * time.Second).Unix(),
		DestroyStartedAtUnix: time.Now().Add(-25 * time.Second).Unix(),
	}); err != nil {
		t.Fatalf("UpdateLaunchTerminationIntent: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	err = SubmitJobsToInstance(context.Background(), database, nil, instanceID, []*db.Job{job})
	if err == nil {
		t.Fatal("expected SubmitJobsToInstance to reject self-destructing instance")
	}
}

func TestSubmitJobsToInstanceDoesNotAssociateJobsWithoutAck(t *testing.T) {
	database := db.SetupTestDB(t)

	graceDeadline := time.Now().Add(5 * time.Minute).Unix()
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:        db.LaunchStatusGrace,
		Provider:      "vastai",
		GPUSpec:       "RTX_3090",
		GraceDeadline: &graceDeadline,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevUpload := uploadSourceToR2
	prevSend := sendGraceJobPayload
	t.Cleanup(func() {
		uploadSourceToR2 = prevUpload
		sendGraceJobPayload = prevSend
	})

	uploadSourceToR2 = func(context.Context, *r2.Client, string, []string) (string, error) {
		return "sources/test.tar.gz", nil
	}
	sendGraceJobPayload = func(context.Context, controlplane.GraceStore, int64, controlplane.GraceJobsRequest) (*controlplane.GraceCommandAck, error) {
		return nil, errors.New("ack timeout")
	}

	err = SubmitJobsToInstance(context.Background(), database, nil, instanceID, []*db.Job{job})
	if err == nil {
		t.Fatal("expected SubmitJobsToInstance to fail when the control plane does not ack")
	}

	reloaded, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID reload: %v", err)
	}
	if reloaded.LaunchID != nil {
		t.Fatalf("LaunchID = %v, want nil after failed submission", *reloaded.LaunchID)
	}
}

func TestSubmitJobsToInstanceIncludesArtifactMetadata(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Seed a rental producer (different instance, already terminated) so the
	// classifier treats the consumer's --needs spec as a cross-instance
	// staging request rather than same-instance co-location.
	producerInstanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch producer: %v", err)
	}
	producerID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "echo producer", "producer", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU producer: %v", err)
	}
	if err := db.SetJobLaunchID(database, producerID, producerInstanceID); err != nil {
		t.Fatalf("SetJobLaunchID producer: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	needsSpec := fmt.Sprintf("inputs/data.csv:%d", producerID)
	if err := db.SetJobOutputDirs(database, jobID, []string{"results/"}); err != nil {
		t.Fatalf("SetJobOutputDirs: %v", err)
	}
	if err := db.SetJobProduces(database, jobID, []string{"results/model.pt"}); err != nil {
		t.Fatalf("SetJobProduces: %v", err)
	}
	if err := db.SetJobNeeds(database, jobID, []string{needsSpec}); err != nil {
		t.Fatalf("SetJobNeeds: %v", err)
	}
	if err := db.SetJobInputs(database, jobID, []string{"local:data/conllu/"}); err != nil {
		t.Fatalf("SetJobInputs: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevUpload := uploadSourceToR2
	prevSendNoAck := sendGraceJobPayloadNoAck
	prevResolveCloudNeeds := resolveCloudNeedsFunc
	t.Cleanup(func() {
		uploadSourceToR2 = prevUpload
		sendGraceJobPayloadNoAck = prevSendNoAck
		resolveCloudNeedsFunc = prevResolveCloudNeeds
	})

	var uploadedInputs []string
	uploadSourceToR2 = func(_ context.Context, _ *r2.Client, _ string, inputs []string) (string, error) {
		uploadedInputs = append([]string(nil), inputs...)
		return "sources/test.tar.gz", nil
	}

	var got controlplane.GraceJobsRequest
	sendGraceJobPayloadNoAck = func(_ context.Context, _ controlplane.GraceStore, _ int64, payload controlplane.GraceJobsRequest) error {
		got = payload
		return nil
	}
	resolveCloudNeedsFunc = func(_ context.Context, _ *sql.DB, _ *r2.Client, _ []string) ([]cloud.CloudNeed, error) {
		return []cloud.CloudNeed{{
			Spec:  needsSpec,
			Path:  "inputs/data.csv",
			R2Key: fmt.Sprintf("jobs/%d/runs/0/artifacts/files/inputs/data.csv", producerID),
		}}, nil
	}

	if err := SubmitJobsToInstance(context.Background(), database, nil, instanceID, []*db.Job{job}); err != nil {
		t.Fatalf("SubmitJobsToInstance: %v", err)
	}
	if len(got.Jobs) != 1 {
		t.Fatalf("jobs len = %d, want 1", len(got.Jobs))
	}
	if got.Jobs[0].RunID <= 0 {
		t.Fatalf("run_id = %d, want non-zero", got.Jobs[0].RunID)
	}
	if !reflect.DeepEqual(got.Jobs[0].OutputDirs, []string{"results/"}) {
		t.Fatalf("output dirs = %v", got.Jobs[0].OutputDirs)
	}
	if !reflect.DeepEqual(got.Jobs[0].Produces, []string{"results/model.pt"}) {
		t.Fatalf("produces = %v", got.Jobs[0].Produces)
	}
	if !reflect.DeepEqual(got.Jobs[0].Needs, []string{needsSpec}) {
		t.Fatalf("needs = %v", got.Jobs[0].Needs)
	}
	if !reflect.DeepEqual(got.Jobs[0].CloudNeeds, []cloud.CloudNeed{{
		Spec:  needsSpec,
		Path:  "inputs/data.csv",
		R2Key: fmt.Sprintf("jobs/%d/runs/0/artifacts/files/inputs/data.csv", producerID),
	}}) {
		t.Fatalf("cloud needs = %v", got.Jobs[0].CloudNeeds)
	}
	if len(got.Sources) != 1 {
		t.Fatalf("sources len = %d, want 1", len(got.Sources))
	}
	if got.Sources[0].R2Key != "sources/test.tar.gz" {
		t.Fatalf("source r2 key = %q", got.Sources[0].R2Key)
	}
	if got.Sources[0].RemoteDir != "/workspace/project" {
		t.Fatalf("source remote dir = %q", got.Sources[0].RemoteDir)
	}
	if !reflect.DeepEqual(uploadedInputs, []string{"local:data/conllu/"}) {
		t.Fatalf("uploaded inputs = %v", uploadedInputs)
	}
}

func TestSubmitJobsToInstanceRollsBackAllClaimsOnNoAckFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobAID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project-a", "python a.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU A: %v", err)
	}
	jobBID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project-b", "python b.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU B: %v", err)
	}
	jobA, err := db.GetJobByID(database, jobAID)
	if err != nil {
		t.Fatalf("GetJobByID A: %v", err)
	}
	jobB, err := db.GetJobByID(database, jobBID)
	if err != nil {
		t.Fatalf("GetJobByID B: %v", err)
	}

	prevUpload := uploadSourceToR2
	prevSendNoAck := sendGraceJobPayloadNoAck
	t.Cleanup(func() {
		uploadSourceToR2 = prevUpload
		sendGraceJobPayloadNoAck = prevSendNoAck
	})

	uploadSourceToR2 = func(_ context.Context, _ *r2.Client, sourceDir string, _ []string) (string, error) {
		return sourceDir + ".tar.gz", nil
	}
	sendGraceJobPayloadNoAck = func(context.Context, controlplane.GraceStore, int64, controlplane.GraceJobsRequest) error {
		return errors.New("write failed")
	}

	err = SubmitJobsToInstance(context.Background(), database, nil, instanceID, []*db.Job{jobA, jobB})
	if err == nil {
		t.Fatal("expected submit error")
	}

	reloadedA, err := db.GetJobByID(database, jobAID)
	if err != nil {
		t.Fatalf("GetJobByID reload A: %v", err)
	}
	reloadedB, err := db.GetJobByID(database, jobBID)
	if err != nil {
		t.Fatalf("GetJobByID reload B: %v", err)
	}
	if reloadedA.LaunchID != nil {
		t.Fatalf("job A LaunchID = %v, want nil", *reloadedA.LaunchID)
	}
	if reloadedB.LaunchID != nil {
		t.Fatalf("job B LaunchID = %v, want nil", *reloadedB.LaunchID)
	}
}
