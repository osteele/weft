package db

import (
	"testing"
	"time"
)

func TestObserveQueueWorkerAbsentBoundsUnknownAndReleasesCapacity(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "host-alpha", "/tmp", "true", "unknown outcome")
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.LatestRunID == nil {
		t.Fatal("running job has no attempt id")
	}
	attemptID := *job.LatestRunID
	bound := 15 * time.Minute
	t0 := time.Unix(2_000_000, 0)

	becameUnresolved, err := ObserveQueueWorkerAbsent(database, jobID, attemptID, t0, bound, "worker absent")
	if err != nil {
		t.Fatal(err)
	}
	if becameUnresolved {
		t.Fatal("first absent observation resolved the outcome before the bound")
	}
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusRunning {
		t.Fatalf("status after first observation = %q, want running", job.Status)
	}
	if job.Metadata == nil || job.Metadata.Reconciliation == nil || job.Metadata.Reconciliation.StatusUnknownSince != t0.Unix() {
		t.Fatalf("unknown-since metadata = %+v, want %d", job.Metadata, t0.Unix())
	}

	becameUnresolved, err = ObserveQueueWorkerAbsent(database, jobID, attemptID, t0.Add(bound-time.Second), bound, "worker absent")
	if err != nil {
		t.Fatal(err)
	}
	if becameUnresolved {
		t.Fatal("absent observation resolved the outcome before the full bound")
	}
	becameUnresolved, err = ObserveQueueWorkerAbsent(database, jobID, attemptID, t0.Add(bound), bound, "worker absent")
	if err != nil {
		t.Fatal(err)
	}
	if !becameUnresolved {
		t.Fatal("absence sustained through the bound did not become unresolved")
	}

	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusUnresolved {
		t.Fatalf("status = %q, want unresolved", job.Status)
	}
	if job.LatestRunID == nil || *job.LatestRunID != attemptID {
		t.Fatalf("attempt id = %v, want unchanged %d", job.LatestRunID, attemptID)
	}
	if job.EndTime != nil || job.ExitCode != nil {
		t.Fatalf("unresolved attempt fabricated terminal fields: end=%v exit=%v", job.EndTime, job.ExitCode)
	}
	if job.LastSyncedStatus != StatusRunning {
		t.Fatalf("last_synced_status = %q, want preserved running evidence", job.LastSyncedStatus)
	}
	if job.Metadata == nil || job.Metadata.Reconciliation == nil || job.Metadata.Reconciliation.UnresolvedReason != "worker absent" {
		t.Fatalf("unresolved metadata = %+v", job.Metadata)
	}

	active, err := CountQueueRunnerActiveByHost(database, "host-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("queue-runner active count = %d, want 0", active)
	}
	placementJobs, err := ListActiveJobs(database, "host-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(placementJobs) != 0 {
		t.Fatalf("placement active jobs = %d, want 0", len(placementJobs))
	}
	reconcileJobs, err := ListJobsForReconciliation(database, "host-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(reconcileJobs) != 1 || reconcileJobs[0].ID != jobID {
		t.Fatalf("reconciliation jobs = %+v, want job %d", reconcileJobs, jobID)
	}
	onPremJobs, err := ListActiveOnPremJobs(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(onPremJobs) != 1 || onPremJobs[0].ID != jobID {
		t.Fatalf("on-prem display jobs = %+v, want unresolved job %d", onPremJobs, jobID)
	}
	for name, listHosts := range map[string]func(*testing.T) []string{
		"running": func(t *testing.T) []string {
			hosts, err := ListUniqueRunningHosts(database)
			if err != nil {
				t.Fatal(err)
			}
			return hosts
		},
		"active": func(t *testing.T) []string {
			hosts, err := ListUniqueActiveHosts(database)
			if err != nil {
				t.Fatal(err)
			}
			return hosts
		},
		"queue-runner": func(t *testing.T) []string {
			hosts, err := ListHostsWithQueueRunnerJobs(database)
			if err != nil {
				t.Fatal(err)
			}
			return hosts
		},
	} {
		t.Run(name, func(t *testing.T) {
			hosts := listHosts(t)
			if len(hosts) != 1 || hosts[0] != "host-alpha" {
				t.Fatalf("hosts = %v, want [host-alpha]", hosts)
			}
		})
	}
}

func TestObserveQueueWorkerAbsentCannotChangeNewerAttempt(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "host-alpha", "/tmp", "true", "retried job")
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	old, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if old.LatestRunID == nil {
		t.Fatal("old attempt id is nil")
	}
	oldAttemptID := *old.LatestRunID
	if err := CloseAttempt(database, jobID, StatusFailed, nil, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := RequeueFreshAttemptByTarget(database, jobID, "host-alpha", nil); err != nil {
		t.Fatal(err)
	}
	if err := MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatal(err)
	}

	becameUnresolved, err := ObserveQueueWorkerAbsent(database, jobID, oldAttemptID, time.Now(), time.Second, "stale observation")
	if err != nil {
		t.Fatal(err)
	}
	if becameUnresolved {
		t.Fatal("stale attempt observation changed the current attempt")
	}
	current, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusRunning || current.LatestRunID == nil || *current.LatestRunID == oldAttemptID {
		t.Fatalf("current attempt changed: status=%q run=%v old=%d", current.Status, current.LatestRunID, oldAttemptID)
	}
	if current.Metadata != nil && current.Metadata.Reconciliation != nil && current.Metadata.Reconciliation.StatusUnknownSince != 0 {
		t.Fatalf("stale observation wrote current metadata: %+v", current.Metadata.Reconciliation)
	}
}

func TestPositiveRunnerEvidenceRestoresUnresolvedAttempt(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "host-alpha", "/tmp", "true", "restored worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	attemptID := *job.LatestRunID
	t0 := time.Unix(2_000_000, 0)
	if _, err := ObserveQueueWorkerAbsent(database, jobID, attemptID, t0, time.Second, "worker absent"); err != nil {
		t.Fatal(err)
	}
	if became, err := ObserveQueueWorkerAbsent(database, jobID, attemptID, t0.Add(time.Second), time.Second, "worker absent"); err != nil || !became {
		t.Fatalf("mark unresolved: became=%v err=%v", became, err)
	}
	if err := MarkRunningFromUnresolved(database, jobID); err != nil {
		t.Fatal(err)
	}
	if err := ClearQueueWorkerAbsentObservation(database, jobID, attemptID); err != nil {
		t.Fatal(err)
	}

	restored, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != StatusRunning || restored.LastSyncedStatus != StatusRunning {
		t.Fatalf("restored state = status %q synced %q", restored.Status, restored.LastSyncedStatus)
	}
	if restored.LatestRunID == nil || *restored.LatestRunID != attemptID {
		t.Fatalf("restored attempt id = %v, want %d", restored.LatestRunID, attemptID)
	}
	if restored.Metadata == nil || restored.Metadata.Reconciliation == nil {
		t.Fatal("reconciliation history was discarded")
	}
	if restored.Metadata.Reconciliation.StatusUnknownSince != 0 {
		t.Fatalf("status_unknown_since = %d, want cleared", restored.Metadata.Reconciliation.StatusUnknownSince)
	}
	if restored.Metadata.Reconciliation.UnresolvedAt == 0 {
		t.Fatal("historical unresolved timestamp was discarded")
	}
}
