package placement

import (
	"testing"

	dbpkg "github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
)

// hostAlpha returns the heterogeneous test host: 2x A100 (indices 0,1) +
// 8x RTX 2080 Ti (indices 2-9).
func hostAlpha(t *testing.T) inventory.HostSpec {
	t.Helper()
	for _, h := range inventory.TestHosts() {
		if h.Name == "host-alpha" {
			return h
		}
	}
	t.Fatal("host-alpha not in test fixtures")
	return inventory.HostSpec{}
}

func jobWithDevices(id int64, devices string) *dbpkg.Job {
	return &dbpkg.Job{
		ID:       id,
		GPUClass: "a100",
		Metadata: &dbpkg.JobMetadata{Resource: &dbpkg.ResourceUsage{GPUDevices: devices}},
	}
}

func jobWithClassCount(id int64, class string, count int) *dbpkg.Job {
	j := &dbpkg.Job{ID: id, GPUClass: class}
	if count > 1 {
		j.CLIResourceOverrides = &dbpkg.CLIResourceOverrides{GPUCount: &count}
	}
	return j
}

func TestFreeGPUCapacity_NoActiveJobs(t *testing.T) {
	host := hostAlpha(t)
	free, matching, reserved := FreeGPUCapacity(host, Constraints{GPUClass: "a100"}, nil)
	if free != 2 || matching != 2 || reserved != 0 {
		t.Fatalf("free=%d matching=%d reserved=%d, want 2/2/0", free, matching, reserved)
	}
}

func TestFreeGPUCapacity_CPUOnlyJobsReserveNothing(t *testing.T) {
	host := hostAlpha(t)
	active := []*dbpkg.Job{{ID: 1}, {ID: 2, Command: "echo hi"}}
	free, matching, reserved := FreeGPUCapacity(host, Constraints{GPUClass: "a100"}, active)
	if free != 2 || matching != 2 || reserved != 0 {
		t.Fatalf("free=%d matching=%d reserved=%d, want 2/2/0", free, matching, reserved)
	}
}

func TestFreeGPUCapacity_ConcreteDevicesReduceOnlyTheirPool(t *testing.T) {
	host := hostAlpha(t)
	// Running job holds A100 device 0.
	active := []*dbpkg.Job{jobWithDevices(1, "0")}

	free, matching, reserved := FreeGPUCapacity(host, Constraints{GPUClass: "a100"}, active)
	if free != 1 || matching != 2 || reserved != 1 {
		t.Fatalf("a100: free=%d matching=%d reserved=%d, want 1/2/1", free, matching, reserved)
	}

	// The 2080 Ti pool (indices 2-9) is untouched by a device-0 reservation.
	free, matching, reserved = FreeGPUCapacity(host, Constraints{GPUClass: "rtx2080ti"}, active)
	if free != 8 || matching != 8 || reserved != 0 {
		t.Fatalf("rtx2080ti: free=%d matching=%d reserved=%d, want 8/8/0", free, matching, reserved)
	}
}

func TestFreeGPUCapacity_DuplicateDeviceClaimsCountOnce(t *testing.T) {
	host := hostAlpha(t)
	active := []*dbpkg.Job{jobWithDevices(1, "0"), jobWithDevices(2, "0")}
	free, _, reserved := FreeGPUCapacity(host, Constraints{GPUClass: "a100"}, active)
	if free != 1 || reserved != 1 {
		t.Fatalf("free=%d reserved=%d, want 1/1 (stale duplicate claim deduplicated)", free, reserved)
	}
}

func TestFreeGPUCapacity_UnknownDevicesDegradeToRequestedCount(t *testing.T) {
	host := hostAlpha(t)
	// Queued 2-GPU A100 job: no concrete devices yet, reserves its count.
	active := []*dbpkg.Job{jobWithClassCount(1, "a100", 2)}

	free, matching, reserved := FreeGPUCapacity(host, Constraints{GPUClass: "a100"}, active)
	if free != 0 || matching != 2 || reserved != 2 {
		t.Fatalf("a100: free=%d matching=%d reserved=%d, want 0/2/2", free, matching, reserved)
	}

	// The A100 reservation does not overlap the 2080 Ti pool.
	free, matching, reserved = FreeGPUCapacity(host, Constraints{GPUClass: "rtx2080ti"}, active)
	if free != 8 || matching != 8 || reserved != 0 {
		t.Fatalf("rtx2080ti: free=%d matching=%d reserved=%d, want 8/8/0", free, matching, reserved)
	}
}

func TestFreeGPUCapacity_ClasslessReservationOverlapsAllPools(t *testing.T) {
	host := hostAlpha(t)
	mem := 8
	// Classless GPU job (gpu-mem only) could land on any device.
	active := []*dbpkg.Job{{ID: 1, GPUMemGB: &mem}}

	free, _, reserved := FreeGPUCapacity(host, Constraints{GPUClass: "a100"}, active)
	if free != 1 || reserved != 1 {
		t.Fatalf("a100: free=%d reserved=%d, want 1/1", free, reserved)
	}
	free, _, reserved = FreeGPUCapacity(host, Constraints{GPUClass: "rtx2080ti"}, active)
	if free != 7 || reserved != 1 {
		t.Fatalf("rtx2080ti: free=%d reserved=%d, want 7/1", free, reserved)
	}
}

func TestFreeGPUCapacity_SelfJobExcluded(t *testing.T) {
	host := hostAlpha(t)
	active := []*dbpkg.Job{jobWithClassCount(7, "a100", 2)}
	c := Constraints{GPUClass: "a100", NumGPUs: 2, SelfJobID: 7}
	free, matching, reserved := FreeGPUCapacity(host, c, active)
	if free != 2 || matching != 2 || reserved != 0 {
		t.Fatalf("free=%d matching=%d reserved=%d, want 2/2/0 (own reservation excluded)", free, matching, reserved)
	}
}

func TestFreeGPUCapacity_UnparseableDeviceStringDegradesToCount(t *testing.T) {
	host := hostAlpha(t)
	// A non-numeric device string (e.g. from a non-CUDA runner) means the
	// concrete assignment is unknown; the job falls back to count-based.
	j := jobWithDevices(1, "mps")
	free, _, reserved := FreeGPUCapacity(host, Constraints{GPUClass: "a100"}, []*dbpkg.Job{j})
	if free != 1 || reserved != 1 {
		t.Fatalf("free=%d reserved=%d, want 1/1 (count-based degradation)", free, reserved)
	}
}

func TestScoreHosts_FreeGPUCapacity_RejectsHostWithAllMatchingGPUsReserved(t *testing.T) {
	database := dbpkg.SetupTestDB(t)

	// Two running jobs holding both A100s on host-alpha.
	for _, dev := range []string{"0", "1"} {
		id, err := dbpkg.RecordQueued(database, "host-alpha", "/tmp/project", "python train.py", "occupant "+dev)
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := dbpkg.SetJobGPUClass(database, id, "a100"); err != nil {
			t.Fatalf("SetJobGPUClass: %v", err)
		}
		if err := dbpkg.MarkQueuedJobRunning(database, id); err != nil {
			t.Fatalf("MarkQueuedJobRunning: %v", err)
		}
		meta := &dbpkg.JobMetadata{Resource: &dbpkg.ResourceUsage{GPUDevices: dev}}
		if err := dbpkg.SetJobMetadata(database, id, meta); err != nil {
			t.Fatalf("SetJobMetadata: %v", err)
		}
	}

	scores := scoreTestHosts(database, Constraints{GPUClass: "a100"})
	alpha := findScore(scores, "host-alpha")
	if alpha.Eligible {
		t.Fatalf("host-alpha should be ineligible with both A100s reserved; reasons: %v", alpha.Reasons)
	}
	if !hasReasonContaining(alpha.Reasons, "gpu availability: 0 of 2 matching GPU(s) free") {
		t.Fatalf("expected free-capacity rejection reason, got %v", alpha.Reasons)
	}
}

func TestScoreHosts_FreeGPUCapacity_AllowsHostWithEnoughFreeGPUs(t *testing.T) {
	database := dbpkg.SetupTestDB(t)

	id, err := dbpkg.RecordQueued(database, "host-alpha", "/tmp/project", "python train.py", "occupant")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := dbpkg.SetJobGPUClass(database, id, "a100"); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}
	if err := dbpkg.MarkQueuedJobRunning(database, id); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	meta := &dbpkg.JobMetadata{Resource: &dbpkg.ResourceUsage{GPUDevices: "0"}}
	if err := dbpkg.SetJobMetadata(database, id, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}

	scores := scoreTestHosts(database, Constraints{GPUClass: "a100"})
	alpha := findScore(scores, "host-alpha")
	if !alpha.Eligible {
		t.Fatalf("host-alpha should be eligible with one free A100; reasons: %v", alpha.Reasons)
	}
	if !hasReasonContaining(alpha.Reasons, "1 of 2 matching GPU(s) free") {
		t.Fatalf("expected free-capacity pass reason, got %v", alpha.Reasons)
	}
}

func TestScoreHosts_FreeGPUCapacity_MultiGPURejectedWhenFreeBelowRequested(t *testing.T) {
	database := dbpkg.SetupTestDB(t)

	id, err := dbpkg.RecordQueued(database, "host-alpha", "/tmp/project", "python train.py", "occupant")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := dbpkg.SetJobGPUClass(database, id, "a100"); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}
	if err := dbpkg.MarkQueuedJobRunning(database, id); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	meta := &dbpkg.JobMetadata{Resource: &dbpkg.ResourceUsage{GPUDevices: "0"}}
	if err := dbpkg.SetJobMetadata(database, id, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}

	scores := scoreTestHosts(database, Constraints{GPUClass: "a100", NumGPUs: 2})
	alpha := findScore(scores, "host-alpha")
	if alpha.Eligible {
		t.Fatalf("host-alpha should be ineligible for 2xA100 with one reserved; reasons: %v", alpha.Reasons)
	}
	if !hasReasonContaining(alpha.Reasons, "gpu availability: 1 of 2 matching GPU(s) free (1 reserved by active jobs), need 2") {
		t.Fatalf("expected free-capacity rejection reason, got %v", alpha.Reasons)
	}
}

func TestScoreHosts_FreeGPUCapacity_QueuedReservationConvertsWithoutDoubleCount(t *testing.T) {
	database := dbpkg.SetupTestDB(t)

	// Queued job (no assigned devices yet) reserves by requested count.
	id, err := dbpkg.RecordQueued(database, "host-alpha", "/tmp/project", "python train.py", "occupant")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := dbpkg.SetJobGPUClass(database, id, "a100"); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}

	host := hostAlpha(t)
	c := Constraints{GPUClass: "a100"}

	active, err := dbpkg.ListActiveJobs(database, "host-alpha")
	if err != nil {
		t.Fatalf("ListActiveJobs: %v", err)
	}
	free, _, reserved := FreeGPUCapacity(host, c, active)
	if free != 1 || reserved != 1 {
		t.Fatalf("queued: free=%d reserved=%d, want 1/1", free, reserved)
	}

	// The job starts and the runner reports its concrete device: the count
	// reservation becomes the device reservation — still exactly one GPU.
	if err := dbpkg.MarkQueuedJobRunning(database, id); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	meta := &dbpkg.JobMetadata{Resource: &dbpkg.ResourceUsage{GPUDevices: "0"}}
	if err := dbpkg.SetJobMetadata(database, id, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}

	active, err = dbpkg.ListActiveJobs(database, "host-alpha")
	if err != nil {
		t.Fatalf("ListActiveJobs: %v", err)
	}
	free, _, reserved = FreeGPUCapacity(host, c, active)
	if free != 1 || reserved != 1 {
		t.Fatalf("running: free=%d reserved=%d, want 1/1 (no double-count)", free, reserved)
	}
}

func TestScoreHosts_FreeGPUCapacity_TerminalJobsReleaseReservation(t *testing.T) {
	database := dbpkg.SetupTestDB(t)

	id, err := dbpkg.RecordQueued(database, "host-alpha", "/tmp/project", "python train.py", "occupant")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := dbpkg.SetJobGPUClass(database, id, "a100"); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}
	if err := dbpkg.MarkQueuedJobRunning(database, id); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	if err := dbpkg.RecordCompletionByID(database, id, 0, 0); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}

	scores := scoreTestHosts(database, Constraints{GPUClass: "a100", NumGPUs: 2})
	alpha := findScore(scores, "host-alpha")
	if !alpha.Eligible {
		t.Fatalf("host-alpha should be eligible after job completed; reasons: %v", alpha.Reasons)
	}
}

func TestScoreHosts_FreeGPUCapacity_NoActiveJobsBehavesAsBefore(t *testing.T) {
	database := dbpkg.SetupTestDB(t)
	scores := scoreTestHosts(database, Constraints{GPUClass: "a100", NumGPUs: 2})
	alpha := findScore(scores, "host-alpha")
	if !alpha.Eligible {
		t.Fatalf("host-alpha should be eligible with no active jobs; reasons: %v", alpha.Reasons)
	}
	if hasReasonContaining(alpha.Reasons, "gpu availability") {
		t.Fatalf("no availability reason expected without reservations, got %v", alpha.Reasons)
	}
}

func TestScoreHosts_FreeGPUCapacity_MissingJobsSchemaDegradesOpen(t *testing.T) {
	// The minimal placement test DB has no jobs schema; the availability
	// query fails and the gate must degrade to "all matching GPUs free".
	database := setupTestDB(t)
	scores := scoreTestHosts(database, Constraints{GPUClass: "a100"})
	alpha := findScore(scores, "host-alpha")
	if !alpha.Eligible {
		t.Fatalf("host-alpha should stay eligible when active-job data is unavailable; reasons: %v", alpha.Reasons)
	}
}
