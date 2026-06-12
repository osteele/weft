package campaign

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
	weftsync "github.com/osteele/weft/internal/sync"
)

// testProjectDir returns a real working directory for jobs in submit tests:
// the pre-claim source validation walks the tree, so fixtures need one.
func testProjectDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

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

func TestMatchJobToInstance_BroadNVIDIADoesNotReusePremiumAccelerator(t *testing.T) {
	mem42 := 42
	cap := InstanceCapacity{
		Instance: &db.Launch{
			GPUClass:        "H100",
			ResolvedGPUName: "H100 NVL",
			GPUMemGB:        94,
		},
		DiskFreeGB: 100,
	}

	got, reason := MatchJobToInstance(&db.Job{GPUClass: "nvidia", GPUMemGB: &mem42}, cap)
	if got {
		t.Fatalf("broad NVIDIA job matched H100 reuse; reason=%q", reason)
	}

	got, reason = MatchJobToInstance(&db.Job{GPUClass: "h100", GPUMemGB: &mem42}, cap)
	if !got {
		t.Fatalf("explicit H100 job should match H100 reuse: %s", reason)
	}
}

func TestFindReusableInstances_SkipsLaunchAfterDiskFailure(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().Unix()
	graceDeadline := now + 3600
	_, err := database.Exec(
		`INSERT INTO launches (id, status, provider, created_at, launched_at, gpu_class, gpu_mem_gb, disk_gb, grace_started_at, grace_deadline)
		 VALUES (1, 'grace', 'runpod', ?, ?, 'nvidia', 48, 120, ?, ?)`,
		now, now, now, graceDeadline,
	)
	if err != nil {
		t.Fatalf("insert launch: %v", err)
	}
	_, err = database.Exec(
		`INSERT INTO jobs (id, working_dir, command, created_at, tombstoned)
		 VALUES (10, '/workspace/project', 'uv run python train.py', ?, 0)`,
		now,
	)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	_, err = database.Exec(
		`INSERT INTO job_attempts (job_id, attempt_number, launch_id, status, start_time, end_time, exit_code, failure_reason, cloud_outcome)
		 VALUES (10, 1, 1, 'failed', ?, ?, 1, 'disk_full', 'failed')`,
		now, now+10,
	)
	if err != nil {
		t.Fatalf("insert attempt: %v", err)
	}

	instances, err := FindReusableInstances(database)
	if err != nil {
		t.Fatalf("FindReusableInstances: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("FindReusableInstances returned %d instances, want 0", len(instances))
	}
}

func TestFormatReuseAssignmentsShowsEstimatedWait(t *testing.T) {
	assignments := []ReuseAssignment{{
		Job: &db.Job{ID: 1877},
		Instance: InstanceCapacity{
			Instance: &db.Launch{
				ID:       2703,
				Status:   db.LaunchStatusRunning,
				GPUSpec:  "A100",
				GPUMemGB: 80,
			},
			RunningJobCount: 11,
		},
	}}

	got := FormatReuseAssignments(assignments)
	if !strings.Contains(got, "10 job(s) ahead") {
		t.Fatalf("missing jobs-ahead detail: %s", got)
	}
	if !strings.Contains(got, "~5h wait") {
		t.Fatalf("missing estimated wait: %s", got)
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

// TestMatchJobToInstance_GPUMemoryRollsBackPostHeadroom is the wj2265
// regression: a job persisted with the submit-time +2GB headroom baked in
// (gpu_class=a100, gpu_mem_gb=82) must still match an A100 80GB instance
// reporting 80GB capacity. IntendedMemGB rolls 82 back to 80 before the
// "job <= instance" comparison runs.
func TestMatchJobToInstance_GPUMemoryRollsBackPostHeadroom(t *testing.T) {
	mem82 := 82
	job := &db.Job{GPUClass: "a100", GPUMemGB: &mem82}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			GPUClass:        "a100",
			ResolvedGPUName: "A100 SXM4",
			GPUMemGB:        80,
		},
		DiskFreeGB: 100,
	}
	got, reason := MatchJobToInstance(job, cap)
	if !got {
		t.Fatalf("MatchJobToInstance() = false, want true (reason: %s)", reason)
	}
}

func TestMatchJobToInstance_RejectsCUDAFloorFromPlacementConstraints(t *testing.T) {
	job := &db.Job{
		GPUClass: "nvidia",
		CLIResourceOverrides: &db.CLIResourceOverrides{
			MinCUDAVersion: "12.8",
		},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			GPUClass:        "nvidia",
			GPUMemGB:        80,
			CUDAVersion:     12.4,
			ResolvedGPUName: "A100",
		},
		DiskFreeGB: 100,
	}
	got, reason := MatchJobToInstance(job, cap)
	if got {
		t.Fatalf("MatchJobToInstance() = true, want false for CUDA floor")
	}
	if !strings.Contains(reason, "CUDA compatibility insufficient") {
		t.Fatalf("reason = %q, want CUDA compatibility detail", reason)
	}

	cap.Instance.CUDAVersion = 12.8
	got, reason = MatchJobToInstance(job, cap)
	if !got {
		t.Fatalf("MatchJobToInstance() = false, want true after CUDA floor met: %s", reason)
	}
}

func TestMatchJobToInstance_RejectsComputeCapFromPlacementConstraints(t *testing.T) {
	job := &db.Job{
		GPUClass:      "h100",
		MaxComputeCap: "8.0",
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			GPUClass:        "h100",
			GPUMemGB:        80,
			CUDAVersion:     12.8,
			ResolvedGPUName: "H100",
		},
		DiskFreeGB: 100,
	}
	got, reason := MatchJobToInstance(job, cap)
	if got {
		t.Fatalf("MatchJobToInstance() = true, want false for compute cap")
	}
	if !strings.Contains(reason, "compute capability too new") {
		t.Fatalf("reason = %q, want compute-cap detail", reason)
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

func TestMatchJobToInstance_ComputeIntensiveCPUFloor(t *testing.T) {
	t.Setenv("WEFT_COMPUTE_CPU_CORES", "16")

	job := &db.Job{Tags: []string{db.TagComputeIntensive}}
	tests := []struct {
		name  string
		cores int
		want  bool
		part  string
	}{
		{"meets floor", 16, true, ""},
		{"below floor", 8, false, "CPU cores insufficient"},
		{"unknown", 0, false, "CPU cores unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := MatchJobToInstance(job, InstanceCapacity{
				Instance:   &db.Launch{CPUCores: tt.cores, GPUMemGB: 80},
				DiskFreeGB: 100,
			})
			if got != tt.want {
				t.Fatalf("MatchJobToInstance = %t, want %t (reason %q)", got, tt.want, reason)
			}
			if tt.part != "" && !strings.Contains(reason, tt.part) {
				t.Fatalf("reason = %q, want containing %q", reason, tt.part)
			}
		})
	}
}

func TestMatchJobToInstance_ComputeIntensiveCPUFloorEnvOverride(t *testing.T) {
	t.Setenv("WEFT_COMPUTE_CPU_CORES", "24")

	job := &db.Job{Tags: []string{db.TagComputeIntensive}}
	got, reason := MatchJobToInstance(job, InstanceCapacity{
		Instance:   &db.Launch{CPUCores: 16, GPUMemGB: 80},
		DiskFreeGB: 100,
	})
	if got {
		t.Fatalf("MatchJobToInstance = true, want false below env floor")
	}
	if !strings.Contains(reason, "need=24") {
		t.Fatalf("reason = %q, want env floor", reason)
	}
}

func TestReuseSource_PreservesComputeIntensiveTags(t *testing.T) {
	t.Setenv("WEFT_COMPUTE_CPU_CORES", "16")
	database := db.SetupTestDB(t)

	_, err := db.CreateLaunch(database, &db.Launch{
		Status:          db.LaunchStatusRunning,
		Provider:        "vastai",
		GPUClass:        "A100",
		GPUMemGB:        80,
		ResolvedGPUName: "A100",
		CPUCores:        8,
		DiskGB:          100,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	src := &ReuseSource{}
	candidates, err := src.Collect(database, placement.Constraints{
		GPUClass: "A100",
		Tags:     []string{db.TagComputeIntensive},
	}, nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %d, want 0 below CPU floor", len(candidates))
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

func TestFindReusableInstancesExcludesCordoned(t *testing.T) {
	database := db.SetupTestDB(t)

	runningID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(running): %v", err)
	}
	cordonedID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(cordoned): %v", err)
	}
	if err := db.SetLaunchCordoned(database, cordonedID, true, "stale agent"); err != nil {
		t.Fatalf("SetLaunchCordoned: %v", err)
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

	if err := db.SetLaunchCordoned(database, cordonedID, false, ""); err != nil {
		t.Fatalf("SetLaunchCordoned(false): %v", err)
	}
	instances, err = FindReusableInstances(database)
	if err != nil {
		t.Fatalf("FindReusableInstances after uncordon: %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("expected 2 reusable instances after uncordon, got %d", len(instances))
	}
	got, err := db.GetLaunch(database, cordonedID)
	if err != nil || got == nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if got.Cordoned || got.CordonReason != "" || got.CordonedAt != nil {
		t.Fatalf("uncordon did not clear fields: cordoned=%v reason=%q at=%v",
			got.Cordoned, got.CordonReason, got.CordonedAt)
	}

	if err := db.SetLaunchCordoned(database, 999999, true, ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("SetLaunchCordoned on missing id: got %v, want sql.ErrNoRows", err)
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

func TestFindReusableInstancesExcludesStaleAgentHeartbeat(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now()
	readyAt := now.Add(-30 * time.Minute).Unix()

	freshID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(fresh): %v", err)
	}
	staleID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_A6000",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(stale): %v", err)
	}
	if err := db.SetLaunchAgentReadyAtIfUnset(database, freshID, time.Unix(readyAt, 0)); err != nil {
		t.Fatalf("SetLaunchAgentReadyAtIfUnset(fresh): %v", err)
	}
	if err := db.SetLaunchAgentReadyAtIfUnset(database, staleID, time.Unix(readyAt, 0)); err != nil {
		t.Fatalf("SetLaunchAgentReadyAtIfUnset(stale): %v", err)
	}
	if _, err := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:    freshID,
		HeartbeatTS: now.Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState(fresh): %v", err)
	}
	if _, err := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:    staleID,
		HeartbeatTS: now.Add(-25 * time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState(stale): %v", err)
	}

	instances, err := FindReusableInstances(database)
	if err != nil {
		t.Fatalf("FindReusableInstances: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 reusable instance, got %d", len(instances))
	}
	if instances[0].Instance.ID != freshID {
		t.Fatalf("reusable instance id = %d, want %d", instances[0].Instance.ID, freshID)
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

	jobID, err := db.RecordQueuedWithGPU(database, "", testProjectDir(t), "python train.py", "queued", "")
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

	jobID, err := db.RecordQueuedWithGPU(database, "", testProjectDir(t), "python train.py", "queued", "")
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
	producerID, err := db.RecordQueuedWithGPU(database, "", testProjectDir(t), "echo producer", "producer", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU producer: %v", err)
	}
	if err := db.SetJobLaunchID(database, producerID, producerInstanceID); err != nil {
		t.Fatalf("SetJobLaunchID producer: %v", err)
	}

	consumerDir := testProjectDir(t)
	if err := os.MkdirAll(filepath.Join(consumerDir, "data", "conllu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(consumerDir, "data", "conllu", "en.conllu"), []byte("# sent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", consumerDir, "python train.py", "queued", "")
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
	sendGraceJobPayloadNoAck = func(_ context.Context, _ controlplane.GraceStore, _ int64, payload controlplane.GraceJobsRequest) (string, error) {
		got = payload
		return "req-test", nil
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
	sendGraceJobPayloadNoAck = func(context.Context, controlplane.GraceStore, int64, controlplane.GraceJobsRequest) (string, error) {
		return "", errors.New("write failed")
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

func TestSubmitJobsToInstanceRejectsEmptySourceDirBeforeUpload(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", "", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevUpload := uploadSourceToR2
	t.Cleanup(func() { uploadSourceToR2 = prevUpload })
	uploadCalled := false
	uploadSourceToR2 = func(context.Context, *r2.Client, string, []string) (string, error) {
		uploadCalled = true
		return "sources/unexpected.tar.gz", nil
	}

	err = SubmitJobsToInstance(context.Background(), database, nil, instanceID, []*db.Job{job})
	if err == nil {
		t.Fatal("expected empty source directory error")
	}
	if !strings.Contains(err.Error(), "has no local source directory") {
		t.Fatalf("unexpected error: %v", err)
	}
	if uploadCalled {
		t.Fatal("uploadSourceToR2 should not be called for empty source directory")
	}
	reloaded, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID reload: %v", err)
	}
	if reloaded.LaunchID != nil {
		t.Fatalf("LaunchID = %v, want nil after rollback", *reloaded.LaunchID)
	}
}

func TestSubmitJobsToInstanceHonorsCanceledContextAfterClaim(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	workDir := t.TempDir()
	jobID, err := db.RecordQueuedWithGPU(database, "", workDir, "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevUpload := uploadSourceToR2
	prevSendNoAck := sendGraceJobPayloadNoAck
	t.Cleanup(func() {
		uploadSourceToR2 = prevUpload
		sendGraceJobPayloadNoAck = prevSendNoAck
	})
	uploadSourceToR2 = func(ctx context.Context, _ *r2.Client, _ string, _ []string) (string, error) {
		return "", ctx.Err()
	}
	sendGraceJobPayloadNoAck = func(ctx context.Context, _ controlplane.GraceStore, _ int64, _ controlplane.GraceJobsRequest) (string, error) {
		t.Fatal("grace payload should not be sent after canceled upload")
		return "", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = SubmitJobsToInstance(ctx, database, nil, instanceID, []*db.Job{job})
	if err == nil {
		t.Fatal("expected canceled context error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestSubmitJobsToInstanceForMove_SupersedesActiveSourceClaim(t *testing.T) {
	// Regression: the move-to-existing path skips the explicit "unplace
	// source first" step. SubmitJobsToInstanceForMove must succeed even
	// when the job is currently claimed by an active source launch — the
	// transfer-claim atomically closes the source attempt and creates a
	// new attempt on the destination. See specs/job-move.allium § move
	// flow.
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch dst: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", testProjectDir(t), "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	sourceAttemptID, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	if _, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:           jobID,
		SourceAttemptID: &sourceAttemptID,
		SourceLaunchID:  &src,
		TargetKind:      db.MoveTargetExisting,
		TargetLaunchID:  &dst,
	}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	// Plain SubmitJobsToInstance must reject because the source is active.
	prevUpload := uploadSourceToR2
	prevSendNoAck := sendGraceJobPayloadNoAck
	t.Cleanup(func() {
		uploadSourceToR2 = prevUpload
		sendGraceJobPayloadNoAck = prevSendNoAck
	})
	uploadSourceToR2 = func(context.Context, *r2.Client, string, []string) (string, error) {
		return "sources/x.tar.gz", nil
	}
	sendGraceJobPayloadNoAck = func(context.Context, controlplane.GraceStore, int64, controlplane.GraceJobsRequest) (string, error) {
		return "req-test", nil
	}

	if err := SubmitJobsToInstance(context.Background(), database, nil, dst, []*db.Job{job}); err == nil {
		t.Fatal("plain SubmitJobsToInstance should reject claim of job already on active source")
	}

	if err := SubmitJobsToInstanceForMove(context.Background(), database, nil, dst, []*db.Job{job}); err != nil {
		t.Fatalf("SubmitJobsToInstanceForMove: %v", err)
	}
	reloaded, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID reload: %v", err)
	}
	if reloaded.LaunchID == nil || *reloaded.LaunchID != src {
		t.Fatalf("before target ack: launch_id = %v, want source %d", reloaded.LaunchID, src)
	}
	intent, err := db.GetOpenMoveIntent(database, jobID)
	if err != nil {
		t.Fatalf("GetOpenMoveIntent: %v", err)
	}
	if intent == nil {
		t.Fatal("move intent should remain open until target ack")
	}
	if intent.TargetRequestID != "req-test" {
		t.Fatalf("target request id = %q, want req-test", intent.TargetRequestID)
	}
	var sourceReason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, sourceAttemptID).Scan(&sourceReason); err != nil {
		t.Fatalf("source abandoned reason: %v", err)
	}
	if sourceReason != "" {
		t.Fatalf("source abandoned reason = %q, want pending source", sourceReason)
	}
}

func TestSubmitJobsToInstanceForMove_RunningTargetWaitsForAckBeforeSourceCancel(t *testing.T) {
	// A running destination only drains grace requests between jobs. The
	// source must remain authoritative until sync observes the target ack.
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch dst: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", testProjectDir(t), "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	// Capture the source attempt id so we can assert the marker carried it.
	var srcAttemptID int64
	if err := database.QueryRow(`SELECT id FROM job_attempts WHERE job_id = ? AND end_time IS NULL`, jobID).Scan(&srcAttemptID); err != nil {
		t.Fatalf("source attempt: %v", err)
	}
	if _, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:           jobID,
		SourceAttemptID: &srcAttemptID,
		SourceLaunchID:  &src,
		TargetKind:      db.MoveTargetExisting,
		TargetLaunchID:  &dst,
	}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevUpload := uploadSourceToR2
	prevSendNoAck := sendGraceJobPayloadNoAck
	prevSendCancel := sendGraceCancelAttempts
	t.Cleanup(func() {
		uploadSourceToR2 = prevUpload
		sendGraceJobPayloadNoAck = prevSendNoAck
		sendGraceCancelAttempts = prevSendCancel
	})
	uploadSourceToR2 = func(context.Context, *r2.Client, string, []string) (string, error) {
		return "sources/x.tar.gz", nil
	}
	sendGraceJobPayloadNoAck = func(context.Context, controlplane.GraceStore, int64, controlplane.GraceJobsRequest) (string, error) {
		return "req-test", nil
	}
	canceledByLaunch := map[int64][]int64{}
	sendGraceCancelAttempts = func(_ context.Context, _ controlplane.GraceStore, instanceID int64, attemptIDs []int64) error {
		canceledByLaunch[instanceID] = append(canceledByLaunch[instanceID], attemptIDs...)
		return nil
	}

	if err := SubmitJobsToInstanceForMove(context.Background(), database, &r2.Client{}, dst, []*db.Job{job}); err != nil {
		t.Fatalf("SubmitJobsToInstanceForMove: %v", err)
	}
	if len(canceledByLaunch) != 0 {
		t.Fatalf("cancel-attempts before target ack = %v, want none", canceledByLaunch)
	}
	intent, err := db.GetOpenMoveIntent(database, jobID)
	if err != nil {
		t.Fatalf("GetOpenMoveIntent: %v", err)
	}
	if intent == nil {
		t.Fatal("move intent should remain open")
	}
	if intent.TargetRequestID != "req-test" {
		t.Fatalf("target request id = %q, want req-test", intent.TargetRequestID)
	}
}

func TestSubmitJobsToInstanceForMove_NoAckStatusFlipStillWaitsForAck(t *testing.T) {
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch dst: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", testProjectDir(t), "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	srcAttemptID, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID: %v", err)
	}
	if _, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:           jobID,
		SourceAttemptID: &srcAttemptID,
		SourceLaunchID:  &src,
		TargetKind:      db.MoveTargetExisting,
		TargetLaunchID:  &dst,
	}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevUpload := uploadSourceToR2
	prevSendNoAck := sendGraceJobPayloadNoAck
	prevSendCancel := sendGraceCancelAttempts
	t.Cleanup(func() {
		uploadSourceToR2 = prevUpload
		sendGraceJobPayloadNoAck = prevSendNoAck
		sendGraceCancelAttempts = prevSendCancel
	})
	uploadSourceToR2 = func(context.Context, *r2.Client, string, []string) (string, error) {
		return "sources/x.tar.gz", nil
	}
	sendGraceJobPayloadNoAck = func(context.Context, controlplane.GraceStore, int64, controlplane.GraceJobsRequest) (string, error) {
		if err := db.SetLaunchGraceStarted(database, dst, time.Now().Add(5*time.Minute).Unix()); err != nil {
			t.Fatalf("SetLaunchGraceStarted: %v", err)
		}
		return "req-flip", nil
	}
	canceledByLaunch := map[int64][]int64{}
	sendGraceCancelAttempts = func(_ context.Context, _ controlplane.GraceStore, instanceID int64, attemptIDs []int64) error {
		canceledByLaunch[instanceID] = append(canceledByLaunch[instanceID], attemptIDs...)
		return nil
	}

	if err := SubmitJobsToInstanceForMove(context.Background(), database, &r2.Client{}, dst, []*db.Job{job}); err != nil {
		t.Fatalf("SubmitJobsToInstanceForMove: %v", err)
	}
	if len(canceledByLaunch) != 0 {
		t.Fatalf("cancel-attempts before target ack = %v, want none", canceledByLaunch)
	}
	intent, err := db.GetOpenMoveIntent(database, jobID)
	if err != nil {
		t.Fatalf("GetOpenMoveIntent: %v", err)
	}
	if intent == nil {
		t.Fatal("move intent should remain open")
	}
	if intent.TargetRequestID != "req-flip" {
		t.Fatalf("target request id = %q, want req-flip", intent.TargetRequestID)
	}
}

func TestSubmitJobsToInstanceForMove_PersistsRequestBeforeFinalCheckFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch dst: %v", err)
	}

	jobID, err := db.RecordQueuedWithGPU(database, "", testProjectDir(t), "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	srcAttemptID, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID: %v", err)
	}
	if _, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{
		JobID:           jobID,
		SourceAttemptID: &srcAttemptID,
		SourceLaunchID:  &src,
		TargetKind:      db.MoveTargetExisting,
		TargetLaunchID:  &dst,
	}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	prevUpload := uploadSourceToR2
	prevSendNoAck := sendGraceJobPayloadNoAck
	t.Cleanup(func() {
		uploadSourceToR2 = prevUpload
		sendGraceJobPayloadNoAck = prevSendNoAck
	})
	uploadSourceToR2 = func(context.Context, *r2.Client, string, []string) (string, error) {
		return "sources/x.tar.gz", nil
	}
	sendGraceJobPayloadNoAck = func(context.Context, controlplane.GraceStore, int64, controlplane.GraceJobsRequest) (string, error) {
		if err := db.SetLaunchAgentReadyAtIfUnset(database, dst, time.Now()); err != nil {
			t.Fatalf("SetLaunchAgentReadyAtIfUnset: %v", err)
		}
		return "req-before-fail", nil
	}

	err = SubmitJobsToInstanceForMove(context.Background(), database, &r2.Client{}, dst, []*db.Job{job})
	if err == nil {
		t.Fatal("expected final live-state check failure")
	}
	intent, getErr := db.GetOpenMoveIntent(database, jobID)
	if getErr != nil {
		t.Fatalf("GetOpenMoveIntent: %v", getErr)
	}
	if intent == nil {
		t.Fatal("move intent should remain open")
	}
	if intent.TargetRequestID != "req-before-fail" {
		t.Fatalf("target request id = %q, want req-before-fail", intent.TargetRequestID)
	}
}

// Regression for the wj2812 churn (wb18): a deterministic source rejection
// (over the size cap) must fail BEFORE any claim, leaving zero attempt rows —
// the claim-then-rollback cycle fabricated two attempt rows per autopilot
// pass for two days.
func TestSubmitJobsToInstanceValidatesSourceBeforeClaiming(t *testing.T) {
	database := db.SetupTestDB(t)

	dir := t.TempDir()
	data := make([]byte, weftsync.MaxSourceTarballBytes/4+1)
	for i := range 5 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("big%d.bin", i)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", dir, "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	attemptsBefore, err := db.GetLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("GetLaunchAttempts: %v", err)
	}

	err = SubmitJobsToInstance(context.Background(), database, nil, instanceID, []*db.Job{job})
	if err == nil {
		t.Fatal("expected submit to reject oversized source")
	}
	if !errors.Is(err, weftsync.ErrSourceTooLarge) {
		t.Fatalf("error = %v, want ErrSourceTooLarge", err)
	}

	attemptsAfter, err := db.GetLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("GetLaunchAttempts: %v", err)
	}
	if len(attemptsAfter) != len(attemptsBefore) {
		t.Fatalf("attempt rows changed %d -> %d; validation must precede claiming", len(attemptsBefore), len(attemptsAfter))
	}
	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if refreshed.LaunchID != nil {
		t.Fatal("job was claimed onto the launch despite failing validation")
	}
}
