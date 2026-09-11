package runner

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/opsqueue"
)

func stubHostLoad(t *testing.T, load float64) {
	t.Helper()
	original := hostLoadAvg1
	hostLoadAvg1 = func() float64 { return load }
	t.Cleanup(func() { hostLoadAvg1 = original })
}

// Floor-saturated books (idle jobs holding their decayed reservations) must
// not wedge the queue while the host is measured-idle: a default-estimated
// candidate starts at the largest affordable reservation (wb127).
func TestMeasuredLoadAdmissionGrantsAffordableReservation(t *testing.T) {
	r, _ := initTestRunner(t)
	stubHostLoad(t, 0.5) // 0.5*100/8 = 6% < 80 target
	if err := writeJobFile(r.queueDir, &opsqueue.CommandJob{ID: 900, Dir: t.TempDir(), Cmd: "true"}); err != nil {
		t.Fatalf("write running job file: %v", err)
	}
	r.state.AddRunning("900", RunningJobState{LocalAllotment: 30})

	job := &opsqueue.CommandJob{ID: 901, Dir: t.TempDir(), Cmd: "sleep 5"}
	decision := r.evaluateLoadedPendingJob(job.ID, job, false)
	if !decision.canStart {
		t.Fatalf("canStart = false, reason = %q", decision.reason)
	}
	if decision.reason != "" {
		t.Fatalf("reason = %q, want empty for an admitted job", decision.reason)
	}
	// 30 + default 87 (7 cores of 8) > 80; headroom 50, so 50 is granted.
	if decision.allotment != 50 {
		t.Fatalf("granted allotment = %d, want 50", decision.allotment)
	}

	cleanupRunnerProcesses(t, r)
	if err := r.startJob(job.ID, job, nil, decision.allotment); err != nil {
		t.Fatalf("startJob: %v", err)
	}
	state, ok := r.state.GetRunning("901")
	if !ok {
		t.Fatal("admitted job missing from running state")
	}
	if state.LocalAllotment != 50 {
		t.Fatalf("recorded allotment = %d, want 50", state.LocalAllotment)
	}
	if got := r.state.TotalAllotment(); got != 80 {
		t.Fatalf("total allotment = %d, want 80", got)
	}
}

func TestMeasuredLoadAdmissionRefusesWhenHostIsBusy(t *testing.T) {
	r, _ := initTestRunner(t)
	stubHostLoad(t, 8.0) // 8.0*100/8 = 100% >= 80 target
	r.state.AddRunning("900", RunningJobState{LocalAllotment: 30})

	job := &opsqueue.CommandJob{ID: 901, Dir: t.TempDir(), Cmd: "true"}
	decision := r.evaluateLoadedPendingJob(job.ID, job, false)
	if decision.canStart || !strings.HasPrefix(decision.reason, "cpu gate:") {
		t.Fatalf("decision = %+v, want cpu gate block", decision)
	}
}

func TestMeasuredLoadAdmissionRefusesWithoutMinHeadroom(t *testing.T) {
	r, _ := initTestRunner(t)
	stubHostLoad(t, 0.5)
	r.state.AddRunning("900", RunningJobState{LocalAllotment: 73}) // headroom 7 < MinAllotment 10

	job := &opsqueue.CommandJob{ID: 901, Dir: t.TempDir(), Cmd: "true"}
	decision := r.evaluateLoadedPendingJob(job.ID, job, false)
	if decision.canStart || !strings.HasPrefix(decision.reason, "cpu gate:") {
		t.Fatalf("decision = %+v, want cpu gate block", decision)
	}
}

func TestMeasuredLoadAdmissionSkipsExplicitReservations(t *testing.T) {
	r, _ := initTestRunner(t)
	stubHostLoad(t, 0.5)
	r.state.AddRunning("900", RunningJobState{LocalAllotment: 30})

	cpu := 87
	job := &opsqueue.CommandJob{ID: 901, Dir: t.TempDir(), Cmd: "true", CPU: &cpu}
	decision := r.evaluateLoadedPendingJob(job.ID, job, false)
	if decision.canStart || !strings.HasPrefix(decision.reason, "cpu gate:") {
		t.Fatalf("decision = %+v, want cpu gate block for explicit percent", decision)
	}

	job = &opsqueue.CommandJob{ID: 902, Dir: t.TempDir(), Cmd: "true", CPUReserveCores: 9}
	decision = r.evaluateLoadedPendingJob(job.ID, job, false)
	if decision.canStart || !strings.HasPrefix(decision.reason, "cpu gate:") {
		t.Fatalf("decision = %+v, want cpu gate block for explicit reserve", decision)
	}
}

func TestMeasuredLoadAdmissionDisabledWithoutLoadCeiling(t *testing.T) {
	r, _ := initTestRunner(t)
	r.cpuConfig.HostLoadCeiling = 0
	stubHostLoad(t, 0.5)
	r.state.AddRunning("900", RunningJobState{LocalAllotment: 30})

	job := &opsqueue.CommandJob{ID: 901, Dir: t.TempDir(), Cmd: "true"}
	decision := r.evaluateLoadedPendingJob(job.ID, job, false)
	if decision.canStart || !strings.HasPrefix(decision.reason, "cpu gate:") {
		t.Fatalf("decision = %+v, want cpu gate block", decision)
	}
}
