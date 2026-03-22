package campaign

import (
	"context"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &db.Job{GPUClass: tt.jobClass}
			cap := InstanceCapacity{
				Instance: &db.CloudInstance{
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &db.Job{GPUMemGB: tt.jobMemGB}
			cap := InstanceCapacity{
				Instance:   &db.CloudInstance{GPUMemGB: tt.instMemGB},
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
				Instance:          &db.CloudInstance{DiskGB: tt.instDiskGB},
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
		Instance:       &db.CloudInstance{Status: db.CloudInstanceStatusGrace},
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
		Instance: &db.CloudInstance{
			ID:       1,
			Status:   db.CloudInstanceStatusGrace,
			GPUMemGB: 80,
		},
		GraceRemaining:    10 * time.Minute,
		ProvisionedInputs: []string{"hf:model-a"},
		DiskFreeGB:        100,
	}

	runningInstance := InstanceCapacity{
		Instance: &db.CloudInstance{
			ID:       2,
			Status:   db.CloudInstanceStatusRunning,
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

	runningID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance(running): %v", err)
	}
	terminatingID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance(terminating): %v", err)
	}
	if err := db.UpdateCloudInstanceTerminationIntent(database, terminatingID, &instanceintent.Marker{
		TerminalStatus:       db.CloudInstanceStatusCompleted,
		TerminationReason:    db.TerminationReasonCompleted,
		RequestedAtUnix:      time.Now().Add(-30 * time.Second).Unix(),
		DestroyStartedAtUnix: time.Now().Add(-20 * time.Second).Unix(),
	}); err != nil {
		t.Fatalf("UpdateCloudInstanceTerminationIntent: %v", err)
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

	runningID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance(running): %v", err)
	}
	graceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusGrace,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance(grace): %v", err)
	}
	if err := db.SetCloudInstanceGraceStarted(database, graceID, time.Now().Add(10*time.Minute).Unix()); err != nil {
		t.Fatalf("SetCloudInstanceGraceStarted: %v", err)
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
		Instance: &db.CloudInstance{
			ID:       1,
			Status:   db.CloudInstanceStatusGrace,
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

	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	if err := db.UpdateCloudInstanceTerminationIntent(database, instanceID, &instanceintent.Marker{
		TerminalStatus:       db.CloudInstanceStatusCompleted,
		TerminationReason:    db.TerminationReasonCompleted,
		RequestedAtUnix:      time.Now().Add(-35 * time.Second).Unix(),
		DestroyStartedAtUnix: time.Now().Add(-25 * time.Second).Unix(),
	}); err != nil {
		t.Fatalf("UpdateCloudInstanceTerminationIntent: %v", err)
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
