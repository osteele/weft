package ops

import (
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/r2"
	srcsync "github.com/osteele/weft/internal/sync"
)

// TestIsJobInRunnerState_FinishedCountsAsPresent guards the regression that
// re-ran wj2348 on cool30: ensureQueuedJobsOnRemote forward-reconciles by
// re-dispatching synced-queued jobs that aren't visible in the runner's live
// state. Before the archive-on-add fix, a re-dispatched completed job was
// silently dropped by JobCompleted; after that fix it runs again. The fix
// here is to recognize state.Finished as "the runner knows about this job"
// so the forward-reconcile loop skips it (the right action is local-DB
// status reconciliation, not a fresh attempt).
func TestIsJobInRunnerState_FinishedCountsAsPresent(t *testing.T) {
	current := int64(7)
	state := &opsqueue.RunnerState{
		Current: &current,
		Pending: []int64{1, 2},
		Running: map[string]opsqueue.RunnerJobState{
			"3": {StartedAt: 100},
		},
		Finished: map[string]opsqueue.RunnerFinishedState{
			"42": {ExitCode: 0, FinishedAt: 200},
			"43": {ExitCode: 1, FinishedAt: 201},
		},
	}

	cases := []struct {
		jobID int64
		want  bool
		where string
	}{
		{7, true, "current"},
		{1, true, "pending head"},
		{2, true, "pending tail"},
		{3, true, "running"},
		{42, true, "finished (success)"},
		{43, true, "finished (failure)"},
		{99, false, "unknown id"},
	}
	for _, tc := range cases {
		if got := isJobInRunnerState(tc.jobID, state); got != tc.want {
			t.Errorf("isJobInRunnerState(%d) [%s] = %v, want %v", tc.jobID, tc.where, got, tc.want)
		}
	}

	// nil state must remain "not present" so the no-state-file path
	// continues to trigger re-dispatch (see the gate comment in
	// ensureQueuedJobsOnRemote).
	if isJobInRunnerState(1, nil) {
		t.Errorf("isJobInRunnerState(1, nil) = true, want false")
	}
}

// TestShouldRedispatchSyncedJob guards the regression where a failed payload
// probe re-dispatched jobs the runner had already finished. fetchRemoteJobPayloads
// used to fail the entire batch on one malformed line; the old guard gated the whole
// materialization check on payloadErr == nil, so a probe error re-ran every
// synced job — including ones state.json positively shows as Finished. The
// state-only branches (Finished/Running/Current) must NOT depend on the payload
// probe; only the Pending branch may, and an unavailable probe there means
// "unknown", so leave the entry in place.
func TestShouldRedispatchSyncedJob(t *testing.T) {
	runID := int64(7)
	finishedJob := &db.Job{ID: 42, LatestRunID: &runID}
	pendingJob := &db.Job{ID: 5, LatestRunID: &runID}
	absentJob := &db.Job{ID: 99, LatestRunID: &runID}

	state := &opsqueue.RunnerState{
		Pending: []int64{5},
		Finished: map[string]opsqueue.RunnerFinishedState{
			"42": {ExitCode: 0, FinishedAt: 200},
		},
	}
	probeErr := fmt.Errorf("parse remote job payload probe line")

	cases := []struct {
		name           string
		job            *db.Job
		state          *opsqueue.RunnerState
		payloads       map[int64]remoteJobPayload
		payloadErr     error
		wantRedispatch bool
	}{
		{
			name:           "finished job with payload probe error is NOT re-dispatched",
			job:            finishedJob,
			state:          state,
			payloads:       nil,
			payloadErr:     probeErr,
			wantRedispatch: false,
		},
		{
			name:           "pending job with payload probe error is left in place (unknown)",
			job:            pendingJob,
			state:          state,
			payloads:       nil,
			payloadErr:     probeErr,
			wantRedispatch: false,
		},
		{
			name:           "job absent from snapshot with payload probe error is left in place (unknown)",
			job:            absentJob,
			state:          state,
			payloads:       nil,
			payloadErr:     probeErr,
			wantRedispatch: false,
		},
		{
			name:           "job absent from state with confirmed missing payload is re-dispatched",
			job:            absentJob,
			state:          state,
			payloads:       map[int64]remoteJobPayload{99: {observed: true}},
			payloadErr:     nil,
			wantRedispatch: true,
		},
		{
			name:           "job absent from state with valid payload repairs lost admission",
			job:            absentJob,
			state:          state,
			payloads:       map[int64]remoteJobPayload{99: {observed: true, exists: true, runID: 7, runIDOK: true}},
			payloadErr:     nil,
			wantRedispatch: true,
		},
		{
			name:           "pending job with confirmed-missing payload is re-dispatched",
			job:            pendingJob,
			state:          state,
			payloads:       map[int64]remoteJobPayload{5: {observed: true}}, // exists=false: MISSING sentinel
			payloadErr:     nil,
			wantRedispatch: true,
		},
		{
			name:           "pending job with matching payload is materialized",
			job:            pendingJob,
			state:          state,
			payloads:       map[int64]remoteJobPayload{5: {observed: true, exists: true, runID: 7, runIDOK: true}},
			payloadErr:     nil,
			wantRedispatch: false,
		},
		{
			name:           "pending job with stale payload attempt is re-dispatched",
			job:            pendingJob,
			state:          state,
			payloads:       map[int64]remoteJobPayload{5: {observed: true, exists: true, runID: 6, runIDOK: true}},
			payloadErr:     nil,
			wantRedispatch: true,
		},
		{
			name:           "pending job with unreadable run id remains unknown",
			job:            pendingJob,
			state:          state,
			payloads:       map[int64]remoteJobPayload{5: {observed: true, exists: true, detail: "payload run_id is unreadable"}},
			payloadErr:     probeErr,
			wantRedispatch: false,
		},
		{
			name:           "nil state with failed probe remains unknown",
			job:            finishedJob,
			state:          nil,
			payloads:       nil,
			payloadErr:     probeErr,
			wantRedispatch: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRedispatchSyncedJob(tc.job, tc.state, tc.payloads, tc.payloadErr, time.Time{}); got != tc.wantRedispatch {
				t.Errorf("shouldRedispatchSyncedJob = %v, want %v", got, tc.wantRedispatch)
			}
		})
	}
}

// TestShouldRedispatchSyncedJob_WaitsForRunnerToConsumePreviousDispatch pins
// the fence between "the runner does not have this job" and "the runner has
// not looked at our dispatch yet". The runner's cursor is the timestamp of
// the last queue command it applied, stamped by this machine when the command
// was created, so a cursor behind weft's last re-dispatch proves only that the
// dispatch is still in flight. Treating that as a confirmed absence resets a
// live dispatch and re-appends it every pass.
func TestShouldRedispatchSyncedJob_WaitsForRunnerToConsumePreviousDispatch(t *testing.T) {
	runID := int64(7)
	job := &db.Job{ID: 5, LatestRunID: &runID}
	payloads := map[int64]remoteJobPayload{5: {observed: true}} // confirmed missing
	redispatchedAt := time.Date(2026, 9, 24, 0, 11, 0, 0, time.UTC)

	cases := []struct {
		name           string
		cursor         string
		lastRedispatch time.Time
		wantRedispatch bool
		wantUnknown    bool
	}{
		{
			name:           "never re-dispatched acts on the first confirmed absence",
			cursor:         redispatchedAt.Add(-time.Hour).Format(time.RFC3339Nano),
			lastRedispatch: time.Time{},
			wantRedispatch: true,
		},
		{
			name:           "cursor behind the previous re-dispatch is unknown",
			cursor:         redispatchedAt.Add(-30 * time.Second).Format(time.RFC3339Nano),
			lastRedispatch: redispatchedAt,
			wantRedispatch: false,
			wantUnknown:    true,
		},
		{
			name:           "cursor past the previous re-dispatch confirms the loss",
			cursor:         redispatchedAt.Add(time.Second).Format(time.RFC3339Nano),
			lastRedispatch: redispatchedAt,
			wantRedispatch: true,
		},
		{
			name:           "unpublished cursor is unknown, not consumed",
			cursor:         "",
			lastRedispatch: redispatchedAt,
			wantRedispatch: false,
			wantUnknown:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &opsqueue.RunnerState{Cursor: tc.cursor, Pending: []int64{5}}
			got := shouldRedispatchSyncedJob(job, state, payloads, nil, tc.lastRedispatch)
			if got != tc.wantRedispatch {
				t.Errorf("shouldRedispatchSyncedJob = %v, want %v (cursor %q, last re-dispatch %v)",
					got, tc.wantRedispatch, tc.cursor, tc.lastRedispatch)
			}
			reason := syncedJobPublicationUnknownReason(job, state, payloads, nil, tc.lastRedispatch)
			if tc.wantUnknown && !strings.Contains(reason, "has not consumed queue commands") {
				t.Errorf("unknown reason = %q, want the unconsumed-dispatch explanation", reason)
			}
			if !tc.wantUnknown && reason != "" {
				t.Errorf("unknown reason = %q, want none", reason)
			}
		})
	}
}

func TestParseRemoteJobPayloads_PreservesIndependentObservations(t *testing.T) {
	requested := map[int64]struct{}{5: {}, 42: {}, 99: {}}
	got, err := parseRemoteJobPayloads("5\t7\nmalformed\n42\tMISSING\n", requested)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("parse error = %v, want malformed-row diagnostic", err)
	}
	if p := got[5]; !p.observed || !p.exists || !p.runIDOK || p.runID != 7 {
		t.Fatalf("job 5 observation = %+v", p)
	}
	if p := got[42]; !p.observed || p.exists {
		t.Fatalf("job 42 observation = %+v, want confirmed missing", p)
	}
	if p := got[99]; p.observed || p.detail == "" {
		t.Fatalf("job 99 observation = %+v, want unknown with diagnostic", p)
	}
}

func TestRemoteJobPayloadsFromCompleteRunnerState(t *testing.T) {
	runID := int64(7)
	jobs := []*db.Job{
		{ID: 5, LatestRunID: &runID},
		{ID: 42, LatestRunID: &runID},
		{ID: 99, LatestRunID: &runID},
	}
	state := &opsqueue.RunnerState{
		PendingPayloadInventoryComplete: true,
		PendingPayloads: map[string]opsqueue.RunnerPayloadState{
			"5":  {Observation: opsqueue.ObservationPresent, RunID: 7},
			"42": {Observation: opsqueue.ObservationUnknown, Detail: "payload JSON is unreadable"},
		},
	}

	got, err := remoteJobPayloadsFromRunnerState(jobs, state)
	if err == nil || !strings.Contains(err.Error(), "job 42") {
		t.Fatalf("payload conversion error = %v, want job 42 diagnostic", err)
	}
	if payload := got[5]; !payload.observed || !payload.exists || !payload.runIDOK || payload.runID != 7 {
		t.Fatalf("job 5 payload = %+v, want matching present payload", payload)
	}
	if payload := got[42]; !payload.observed || !payload.exists || payload.runIDOK || payload.detail == "" {
		t.Fatalf("job 42 payload = %+v, want present payload with unknown identity", payload)
	}
	if payload := got[99]; !payload.observed || payload.exists {
		t.Fatalf("job 99 payload = %+v, want confirmed missing", payload)
	}
}

func TestRemoteJobPayloadsFromLegacyRunnerStateRemainUnknown(t *testing.T) {
	runID := int64(7)
	jobs := []*db.Job{{ID: 5, LatestRunID: &runID}, {ID: 99, LatestRunID: &runID}}
	state := &opsqueue.RunnerState{
		PendingPayloads: map[string]opsqueue.RunnerPayloadState{
			"5": {Observation: opsqueue.ObservationPresent, RunID: 7},
		},
	}

	got, err := remoteJobPayloadsFromRunnerState(jobs, state)
	if err == nil || !strings.Contains(err.Error(), "does not expose pending payloads") {
		t.Fatalf("payload conversion error = %v, want legacy-state diagnostic", err)
	}
	if payload := got[5]; !payload.observed || !payload.exists || !payload.runIDOK {
		t.Fatalf("job 5 payload = %+v, want independent present observation", payload)
	}
	if payload := got[99]; payload.observed || payload.detail == "" {
		t.Fatalf("job 99 payload = %+v, want unknown omission", payload)
	}
}

// TestShouldPruneStalePending exercises the classifier that decides whether
// a runner-pending entry should be cancelled via host_sync's reverse
// reconcile. The classifier is the contract surface — fixing it wrong
// either cancels legitimate work or leaks stale entries forever, both of
// which have hit production today.
func TestShouldPruneStalePending(t *testing.T) {
	cases := []struct {
		name      string
		job       *db.Job
		host      string
		wantPrune bool
		// reason prefix the classifier should emit for traceability.
		wantReasonPrefix string
	}{
		{
			name:             "nil job (deleted DB row) prunes",
			job:              nil,
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_row_missing",
		},
		{
			name:             "completed job prunes",
			job:              &db.Job{Host: "cool30", Status: db.StatusCompleted},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_terminal:",
		},
		{
			name:             "failed job prunes",
			job:              &db.Job{Host: "cool30", Status: db.StatusFailed},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_terminal:",
		},
		{
			name:             "killed job prunes",
			job:              &db.Job{Host: "cool30", Status: db.StatusKilled},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_terminal:",
		},
		{
			name:             "canceled job prunes",
			job:              &db.Job{Host: "cool30", Status: db.StatusCanceled},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_terminal:",
		},
		{
			name:             "queued on different host prunes",
			job:              &db.Job{Host: "cool100", Status: db.StatusQueued},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_queued_on_other_host",
		},
		{
			name: "queued and retargeted to rental prunes (TargetKind=RentalInstance)",
			job: &db.Job{
				Host:     "cool30",
				Status:   db.StatusQueued,
				LaunchID: int64Ptr(99),
			},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_queued_on_other_host",
		},
		{
			name:             "queued and unplaced prunes (Host cleared)",
			job:              &db.Job{Host: "", Status: db.StatusQueued},
			host:             "cool30",
			wantPrune:        true,
			wantReasonPrefix: "db_queued_on_other_host",
		},
		{
			name:             "queued on same host does NOT prune (defensive)",
			job:              &db.Job{Host: "cool30", Status: db.StatusQueued},
			host:             "cool30",
			wantPrune:        false,
			wantReasonPrefix: "db_queued_here",
		},
		{
			name:             "running job does NOT prune (transient)",
			job:              &db.Job{Host: "cool30", Status: db.StatusRunning},
			host:             "cool30",
			wantPrune:        false,
			wantReasonPrefix: "db_transient:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := shouldPruneStalePending(tc.job, tc.host)
			if got != tc.wantPrune {
				t.Fatalf("prune = %v, want %v (reason=%q)", got, tc.wantPrune, reason)
			}
			if !strings.HasPrefix(reason, tc.wantReasonPrefix) {
				t.Fatalf("reason = %q, want prefix %q", reason, tc.wantReasonPrefix)
			}
		})
	}
}

// stubProbeRemoteNeedsState swaps probeRemoteNeedsStateFunc for the duration
// of a test, recording call count and the last needs slice it received.
func stubProbeRemoteNeedsState(t *testing.T, reply map[string]remoteNeedState) (calls *int, lastNeeds *[]pendingNeed) {
	t.Helper()
	prev := probeRemoteNeedsStateFunc
	t.Cleanup(func() { probeRemoteNeedsStateFunc = prev })
	calls = new(int)
	lastNeeds = new([]pendingNeed)
	probeRemoteNeedsStateFunc = func(_ string, needs []pendingNeed, _ time.Duration) (map[string]remoteNeedState, error) {
		*calls++
		*lastNeeds = needs
		return reply, nil
	}
	return calls, lastNeeds
}

// TestStageArtifactNeedsForHost_SkipsOnPremProducers verifies that when every
// producer is on-prem, the function returns without ever probing or calling
// getR2Client — those needs are satisfied by the producer's own queue runner.
func TestStageArtifactNeedsForHost_SkipsOnPremProducers(t *testing.T) {
	database := db.SetupTestDB(t)

	producerID, err := db.RecordQueued(database, "host-alpha", "/tmp", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}
	consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp", "consume", "consumer")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	if err := db.SetJobNeeds(database, consumerID, []string{fmt.Sprintf("output/x.bin:%d", producerID)}); err != nil {
		t.Fatalf("set needs: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get consumer: %v", err)
	}

	probeCalls, _ := stubProbeRemoteNeedsState(t, nil)
	getR2 := func() (*r2.Client, error) {
		t.Fatal("getR2Client should not be called when all producers are on-prem")
		return nil, nil
	}
	failed := stageArtifactNeedsForHost(database, "host-alpha", []*db.Job{consumer}, time.Second, getR2)
	if len(failed) != 0 {
		t.Fatalf("unexpected failures: %v", failed)
	}
	if *probeCalls != 0 {
		t.Fatalf("probeCalls = %d, want 0 (no rental needs to probe)", *probeCalls)
	}
}

// TestStageArtifactNeedsForHost_NoNeeds verifies the early-return for jobs
// with no --needs entries. No probe, no R2.
func TestStageArtifactNeedsForHost_NoNeeds(t *testing.T) {
	database := db.SetupTestDB(t)
	consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp", "echo", "no-needs")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get consumer: %v", err)
	}
	probeCalls, _ := stubProbeRemoteNeedsState(t, nil)
	getR2 := func() (*r2.Client, error) {
		t.Fatal("getR2Client should not be called for jobs with no --needs")
		return nil, nil
	}
	failed := stageArtifactNeedsForHost(database, "host-alpha", []*db.Job{consumer}, time.Second, getR2)
	if len(failed) != 0 {
		t.Fatalf("unexpected failures: %v", failed)
	}
	if *probeCalls != 0 {
		t.Fatalf("probeCalls = %d, want 0 (no needs)", *probeCalls)
	}
}

// TestStageArtifactNeedsForHost_BatchesProbeAcrossJobs verifies that a
// host-wide stage call issues exactly one probe regardless of how many queued
// jobs need probing — guards against the per-job-probe regression.
func TestStageArtifactNeedsForHost_BatchesProbeAcrossJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	// Producer is a rental job (no host pin → producer.HasInventoryHost() is false).
	producerID, err := db.RecordQueued(database, "", "/tmp", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}

	jobs := make([]*db.Job, 0, 2)
	for i := 0; i < 2; i++ {
		consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp", fmt.Sprintf("consume-%d", i), "consumer")
		if err != nil {
			t.Fatalf("record consumer: %v", err)
		}
		if err := db.SetJobNeeds(database, consumerID, []string{fmt.Sprintf("output/x-%d.bin:%d", i, producerID)}); err != nil {
			t.Fatalf("set needs: %v", err)
		}
		job, err := db.GetJobByID(database, consumerID)
		if err != nil {
			t.Fatalf("get consumer: %v", err)
		}
		jobs = append(jobs, job)
	}

	// Probe replies "marker present" for everything → no transfer, no R2.
	probeCalls, lastNeeds := stubProbeRemoteNeedsState(t, nil)
	probeRemoteNeedsStateFunc = func(_ string, needs []pendingNeed, _ time.Duration) (map[string]remoteNeedState, error) {
		*probeCalls++
		*lastNeeds = needs
		reply := make(map[string]remoteNeedState, len(needs))
		for _, n := range needs {
			reply[n.markerName] = remoteNeedState{markerExists: true, fileSize: 1, stagingSize: -1}
		}
		return reply, nil
	}

	getR2 := func() (*r2.Client, error) {
		t.Fatal("getR2Client should not be called when all markers are present")
		return nil, nil
	}
	failed := stageArtifactNeedsForHost(database, "host-alpha", jobs, time.Second, getR2)
	if len(failed) != 0 {
		t.Fatalf("unexpected failures: %v", failed)
	}
	if *probeCalls != 1 {
		t.Fatalf("probeCalls = %d, want 1 (probe should batch across jobs)", *probeCalls)
	}
	if got := len(*lastNeeds); got != 2 {
		t.Fatalf("probe saw %d needs, want 2 (one per consumer)", got)
	}
}

func TestStageArtifactNeedsForHost_NamedAssetRestagesWhenMarkerExistsButFileMissing(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.UpsertNamedAsset(database, db.NamedAsset{
		Name:        "trace-v1",
		ContentHash: "0123456789abcdef",
		SizeBytes:   123,
		TargetPath:  "data/trace.jsonl",
	}); err != nil {
		t.Fatalf("upsert named asset: %v", err)
	}

	consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "consume", "consumer")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	if err := db.SetJobNeeds(database, consumerID, []string{"asset:trace-v1"}); err != nil {
		t.Fatalf("set needs: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get consumer: %v", err)
	}

	probeCalls, lastNeeds := stubProbeRemoteNeedsState(t, nil)
	probeRemoteNeedsStateFunc = func(_ string, needs []pendingNeed, _ time.Duration) (map[string]remoteNeedState, error) {
		*probeCalls++
		*lastNeeds = needs
		reply := make(map[string]remoteNeedState, len(needs))
		for _, n := range needs {
			reply[n.markerName] = remoteNeedState{markerExists: true, fileSize: -1, stagingSize: -1}
		}
		return reply, nil
	}

	getR2 := func() (*r2.Client, error) {
		return nil, fmt.Errorf("r2 requested")
	}
	failed := stageArtifactNeedsForHost(database, "host-alpha", []*db.Job{consumer}, time.Second, getR2)
	err = failed[consumer.ID]
	if err == nil || !strings.Contains(err.Error(), "r2 requested") {
		t.Fatalf("expected stale marker to trigger staging and fail on R2 setup, got %v", err)
	}
	if *probeCalls != 1 {
		t.Fatalf("probeCalls = %d, want 1", *probeCalls)
	}
	if len(*lastNeeds) != 1 {
		t.Fatalf("probe saw %d needs, want 1", len(*lastNeeds))
	}
	need := (*lastNeeds)[0]
	if need.markerName != "asset-trace-v1.satisfied" {
		t.Fatalf("markerName = %q, want asset marker", need.markerName)
	}
	if need.remotePath != "/tmp/project/data/trace.jsonl" {
		t.Fatalf("remotePath = %q, want target path under working dir", need.remotePath)
	}
}

// TestStageArtifactNeedsForHost_MissingProducer verifies that an unknown
// producer ID surfaces as an error rather than silently being skipped.
func TestStageArtifactNeedsForHost_MissingProducer(t *testing.T) {
	database := db.SetupTestDB(t)
	consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp", "consume", "consumer")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	if err := db.SetJobNeeds(database, consumerID, []string{"output/x.bin:99999"}); err != nil {
		t.Fatalf("set needs: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get consumer: %v", err)
	}
	stubProbeRemoteNeedsState(t, nil)
	getR2 := func() (*r2.Client, error) { return &r2.Client{}, nil }
	failed := stageArtifactNeedsForHost(database, "host-alpha", []*db.Job{consumer}, time.Second, getR2)
	err = failed[consumer.ID]
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error, got %v", err)
	}
}

func TestEnsureQueuedJobsOnRemote(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a queued job with no LastSyncedStatus (simulates job recorded while host offline)
	_, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "unsynced job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Mock sync so rsync doesn't actually run
	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return nil
	}))

	// Mock SSH so AppendJobToQueue and ResolveBackend succeed
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "__WEFT_NO_STATE_FILE__") {
			return currentRunnerStateJSON + "\n", "", 0
		}
		return "", "", 0
	})

	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 1 {
		t.Errorf("expected 1 job ensured, got %d", ensured)
	}
	if !contacted {
		t.Error("expected contacted=true")
	}
}

func TestEnsureQueuedJobsOnRemote_DefersPayloadForIncapableRunner(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "test-host", "", "true", "payload job")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobBackend(database, jobID, db.BackendQueueRunner); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	if err := db.InsertJobPayload(database, db.JobPayload{
		JobID: jobID, Name: "prompt", StoredPath: "payloads/" + digest,
		SizeBytes: 7, SHA256: digest, R2Key: "assets/" + digest,
	}); err != nil {
		t.Fatal(err)
	}
	appendCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "__WEFT_NO_STATE_FILE__") {
			return `{"agent_version":"test-agent","queue_protocol_version":1,"pending":[],"current":null,"capabilities":[]}` + "\n", "", 0
		}
		if strings.Contains(command, `"op":"add"`) {
			appendCount++
		}
		return "", "", 0
	})

	ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", time.Second, time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 || appendCount != 0 {
		t.Fatalf("ensured=%d appendCount=%d, want 0/0", ensured, appendCount)
	}
}

func TestEnsureR2RunnerCapabilitiesInitiatesPayloadUpgrade(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "studio", "", "true", "payload job")
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("b", 64)
	if err := db.InsertJobPayload(database, db.JobPayload{
		JobID: jobID, Name: "prompt", StoredPath: "payloads/" + digest,
		SizeBytes: 7, SHA256: digest, R2Key: "assets/" + digest,
	}); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	started, needed, err := ensureR2RunnerCapabilities(database, "studio", []*db.Job{job}, func(host string) (bool, error) {
		called = host == "studio"
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called || !started || !needed {
		t.Fatalf("called=%v started=%v needed=%v, want true/true/true", called, started, needed)
	}
}

func TestEnsureR2RunnerCapabilitiesInitiatesNamedAssetUpgrade(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "studio", "", "true", "asset job")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobNeeds(database, jobID, []string{"asset:trace"}); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	started, needed, err := ensureR2RunnerCapabilities(database, "studio", []*db.Job{job}, func(host string) (bool, error) {
		called = host == "studio"
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called || !started || !needed {
		t.Fatalf("called=%v started=%v needed=%v, want true/true/true", called, started, needed)
	}
}

func TestEnsureQueuedJobsOnRemote_UsesPinnedClosureAfterWorkingTreeChanges(t *testing.T) {
	database := db.SetupTestDB(t)
	workingDir := t.TempDir()
	pinHash := strings.Repeat("a", 64)
	rootHash := strings.Repeat("b", 64)
	jobID, err := RecordQueuedJob(database, QueueJobParams{
		Host:       "test-host",
		WorkingDir: workingDir,
		Command:    "cat version.txt",
		Metadata: &db.JobMetadata{Source: &db.JobSourceMetadata{Pin: &db.JobSourcePinMetadata{
			Hash: pinHash,
			Roots: []db.JobSourcePinRootMetadata{{
				MountBasename: "project",
				Hash:          rootHash,
				R2Key:         "sources/v2/sha256/" + rootHash + ".tar.gz",
				Blobs: []dataplane.SourceBlob{{
					R2Key: "assets/blob", RelPath: "data/blob.bin", SHA256: strings.Repeat("c", 64),
				}},
			}},
		}}},
	})
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobBackend(database, jobID, db.BackendQueueRunner); err != nil {
		t.Fatalf("set backend: %v", err)
	}

	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		t.Fatal("pinned job consulted the changed live working tree")
		return nil
	}))
	var addCommand string
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return `{"agent_version":"test-agent","queue_protocol_version":1,"pending":[],"current":null}` + "\n", "", 0
		case strings.Contains(command, `"op":"add"`):
			addCommand = command
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 1 {
		t.Fatalf("ensured = %d, want 1", ensured)
	}
	for _, want := range []string{`"source_manifest"`, pinHash, rootHash, `"data/blob.bin"`} {
		if !strings.Contains(addCommand, want) {
			t.Fatalf("queue add command does not contain %q:\n%s", want, addCommand)
		}
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsJobOnSyncFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "sync-fail job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Mock sync to fail
	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return fmt.Errorf("rsync timeout")
	}))

	// Mock SSH — the only legitimate SSH for this test is the runner-state
	// read at the top of ensureQueuedJobsOnRemote (fetchRemoteRunnerState,
	// which both the forward and reverse reconcilers consume). Returning
	// the no-state-file sentinel makes that call a no-op so the prune pass
	// has nothing to do. Any OTHER SSH command would be a job-dispatch
	// path that shouldn't run while source sync is failing.
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if strings.Contains(command, "__WEFT_NO_STATE_FILE__") {
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		}
		t.Errorf("unexpected SSH command when source sync fails: %s", command)
		return "", "", 0
	})

	ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err == nil {
		t.Fatal("expected ensureQueuedJobsOnRemote to surface the sync failure")
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (sync failed), got %d", ensured)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("job %s source sync failed", ids.FormatJobID(jobID))) {
		t.Fatalf("error = %q, want job-specific source sync failure", err)
	}

	// Job should still be unsynced in the database
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.LastSyncedStatus != "" {
		t.Errorf("expected LastSyncedStatus empty (unsynced), got %q", job.LastSyncedStatus)
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsJobOnArtifactStagingFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "asset-needs job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobNeeds(database, jobID, []string{"asset:missing-asset"}); err != nil {
		t.Fatalf("set needs: %v", err)
	}

	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		t.Fatal("source sync should not run when artifact staging fails first")
		return nil
	}))

	appendCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		case strings.Contains(command, `"op":"add"`):
			appendCount++
			t.Errorf("unexpected append after artifact staging failure: %s", command)
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err == nil {
		t.Fatal("expected ensureQueuedJobsOnRemote to surface the artifact staging failure")
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (artifact staging failed), got %d", ensured)
	}
	if appendCount != 0 {
		t.Fatalf("appendCount = %d, want 0", appendCount)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("job %s artifact needs staging failed", ids.FormatJobID(jobID))) {
		t.Fatalf("error = %q, want job-specific artifact staging failure", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.LastSyncedStatus != "" {
		t.Errorf("expected LastSyncedStatus empty (unsynced), got %q", job.LastSyncedStatus)
	}

	var detail string
	if err := database.QueryRow(`
		SELECT COALESCE(detail, '')
		FROM lifecycle_events
		WHERE job_id = ? AND event_kind = ?
		ORDER BY id DESC
		LIMIT 1`, jobID, db.EventQueueDispatchFailed).Scan(&detail); err != nil {
		t.Fatalf("query lifecycle event: %v", err)
	}
	if !strings.Contains(detail, "artifact needs staging failed") {
		t.Fatalf("expected artifact staging lifecycle detail, got %q", detail)
	}
}

// TestEnsureQueuedJobsOnRemote_DefersWhenInDispatchBackoff seeds a job whose
// dispatch-failure run has already aged past the backoff threshold, with its
// most recent real attempt only seconds ago (so it is not yet eligible
// again), and verifies the pass skips it: no SSH append, no new
// EventQueueDispatchFailed row, and a EventQueueDispatchDeferred row instead
// (a deferral is not a failure — recording it as one would compound the run
// it exists to back off from).
func TestEnsureQueuedJobsOnRemote_DefersWhenInDispatchBackoff(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "backed-off job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobNeeds(database, jobID, []string{"asset:missing-asset"}); err != nil {
		t.Fatalf("set needs: %v", err)
	}

	now := time.Now()
	runStart := now.Add(-6 * time.Minute)
	lastAttempt := now.Add(-5 * time.Second)
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, runStart.Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	for _, at := range []time.Time{runStart, lastAttempt} {
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			OccurredAt: at.Unix(),
			EventKind:  db.EventQueueDispatchFailed,
			JobID:      jobID,
			Detail:     "artifact needs staging failed: R2-pull inventory hosts do not yet stage producer-job --needs artifacts",
		}); err != nil {
			t.Fatalf("seed failure at %v: %v", at, err)
		}
	}

	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		t.Fatal("source sync should not run for a job deferred by dispatch backoff")
		return nil
	}))
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		case strings.Contains(command, `"op":"add"`):
			t.Errorf("unexpected append for a job in dispatch backoff: %s", command)
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	before, err := db.LatestLifecycleEventID(database)
	if err != nil {
		t.Fatalf("LatestLifecycleEventID: %v", err)
	}

	ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v, want nil (a deferral is not a failure)", err)
	}
	if ensured != 0 {
		t.Fatalf("ensured = %d, want 0", ensured)
	}

	events, err := db.ListLifecycleEventsAfterID(database, before, 10)
	if err != nil {
		t.Fatalf("ListLifecycleEventsAfterID: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("new events = %d, want exactly 1 (the deferral)", len(events))
	}
	if events[0].EventKind != db.EventQueueDispatchDeferred {
		t.Fatalf("EventKind = %q, want %q", events[0].EventKind, db.EventQueueDispatchDeferred)
	}
	if !strings.Contains(events[0].Detail, "dispatch backoff") {
		t.Fatalf("Detail = %q, want it to name the backoff", events[0].Detail)
	}
}

// TestEnsureQueuedJobsOnRemote_RepeatedFailuresBackOffAtIncreasingIntervals
// is the regression test for the wj8147 bug: a job whose dispatch attempt
// fails with a byte-identical error on every pass must not be re-attempted
// on every pass forever. It exercises three phases against the same
// artifact-staging failure ensureQueuedJobsOnRemote hit in production:
//
//  1. Below the backoff age threshold, every pass is a real attempt and
//     produces its own EventQueueDispatchFailed row — proving the dispatch-
//     failure dedupe trap (Change/host_sync.go recordFailure) no longer
//     collapses the event stream into ~1 row per 5 minutes.
//  2. Once the run's age crosses the threshold, the very next pass is
//     deferred rather than re-attempted, even though it is still "the next
//     pass" — this is the bug fix: dispatch stops trying every pass.
//  3. Once backoff's own delay elapses, dispatch resumes — the deferral is
//     temporary, not a permanent give-up.
func TestEnsureQueuedJobsOnRemote_RepeatedFailuresBackOffAtIncreasingIntervals(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "wj8147-like job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobNeeds(database, jobID, []string{"asset:missing-asset"}); err != nil {
		t.Fatalf("set needs: %v", err)
	}
	// db.DispatchRunFloor falls back to CreatedAt when QueuedAt is unset;
	// backdate it so phase 2's backdated failure event isn't excluded by
	// the run query's floor filter.
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`,
		time.Now().Add(-time.Hour).Unix(), jobID); err != nil {
		t.Fatalf("backdate queued_at: %v", err)
	}

	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		t.Fatal("source sync should not run when artifact staging fails first")
		return nil
	}))
	appendCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		case strings.Contains(command, `"op":"add"`):
			appendCount++
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	failedCount := func() int {
		t.Helper()
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM lifecycle_events WHERE job_id = ? AND event_kind = ?`,
			jobID, db.EventQueueDispatchFailed).Scan(&n); err != nil {
			t.Fatalf("count failed events: %v", err)
		}
		return n
	}
	deferredCount := func() int {
		t.Helper()
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM lifecycle_events WHERE job_id = ? AND event_kind = ?`,
			jobID, db.EventQueueDispatchDeferred).Scan(&n); err != nil {
			t.Fatalf("count deferred events: %v", err)
		}
		return n
	}

	// Phase 1: age well under the 5-minute threshold. Three passes in a
	// row, each a real attempt.
	for i := 1; i <= 3; i++ {
		if _, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default()); err == nil {
			t.Fatalf("pass %d: expected the artifact staging failure to surface", i)
		}
		if got := failedCount(); got != i {
			t.Fatalf("pass %d: failed events = %d, want %d (dedupe must not collapse consecutive identical failures)", i, got, i)
		}
	}
	if appendCount != 0 {
		t.Fatalf("appendCount = %d, want 0 (staging never succeeded)", appendCount)
	}

	// Phase 2: age the run's earliest recorded failure past the 5-minute
	// threshold, leaving the run's most recent event (from phase 1) as-is —
	// i.e. simulate that real time has passed since the last pass. The next
	// pass must defer rather than attempt again.
	if _, err := database.Exec(`
		UPDATE lifecycle_events
		   SET occurred_at = ?
		 WHERE job_id = ? AND event_kind = ?
		   AND id = (SELECT MIN(id) FROM lifecycle_events WHERE job_id = ? AND event_kind = ?)`,
		time.Now().Add(-6*time.Minute).Unix(), jobID, db.EventQueueDispatchFailed, jobID, db.EventQueueDispatchFailed,
	); err != nil {
		t.Fatalf("backdate run start: %v", err)
	}

	if _, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default()); err != nil {
		t.Fatalf("pass 4 (should defer): %v, want nil (a deferral is not a failure)", err)
	}
	if got := failedCount(); got != 3 {
		t.Fatalf("failed events after pass 4 = %d, want still 3 (deferred, not attempted)", got)
	}
	if got := deferredCount(); got != 1 {
		t.Fatalf("deferred events after pass 4 = %d, want 1", got)
	}

	// Phase 3: advance time past the backoff delay (bounded by the cap) with
	// no further attempts recorded — exactly what phase 2's deferral
	// guarantees. Dispatch must resume.
	if _, err := database.Exec(`
		UPDATE lifecycle_events
		   SET occurred_at = occurred_at - ?
		 WHERE job_id = ?`,
		int64((2 * DispatchBackoffCap).Seconds()), jobID,
	); err != nil {
		t.Fatalf("advance clock past backoff: %v", err)
	}

	if _, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default()); err == nil {
		t.Fatal("pass 5 (backoff elapsed): expected the artifact staging failure to surface again")
	}
	if got := failedCount(); got != 4 {
		t.Fatalf("failed events after pass 5 = %d, want 4 (dispatch resumed)", got)
	}
}

func TestEnsureQueuedJobsOnRemote_SourceSyncFailureSkipsSameDirJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	workingDir := t.TempDir()

	jobID1, err := db.RecordQueued(database, "test-host", workingDir, "echo one", "sync-fail one")
	if err != nil {
		t.Fatalf("record queued job 1: %v", err)
	}
	jobID2, err := db.RecordQueued(database, "test-host", workingDir, "echo two", "sync-fail two")
	if err != nil {
		t.Fatalf("record queued job 2: %v", err)
	}

	syncCalls := 0
	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		syncCalls++
		return fmt.Errorf("rsync timeout")
	}))

	appendCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		case strings.Contains(command, `"op":"add"`):
			appendCount++
			t.Errorf("unexpected append after source sync failure: %s", command)
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err == nil {
		t.Fatal("expected ensureQueuedJobsOnRemote to surface the sync failure")
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (sync failed), got %d", ensured)
	}
	if syncCalls != 1 {
		t.Fatalf("syncCalls = %d, want 1 for shared working dir", syncCalls)
	}
	if appendCount != 0 {
		t.Fatalf("appendCount = %d, want 0", appendCount)
	}
	for _, jobID := range []int64{jobID1, jobID2} {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			t.Fatalf("get job %d: %v", jobID, err)
		}
		if job.LastSyncedStatus != "" {
			t.Errorf("job %d LastSyncedStatus = %q, want empty", jobID, job.LastSyncedStatus)
		}
	}
}

func TestEnsureQueuedJobsOnRemote_SourceSyncLeaseContentionDefers(t *testing.T) {
	database := db.SetupTestDB(t)
	workingDir := t.TempDir()

	jobID, err := db.RecordQueued(database, "test-host", workingDir, "echo hello", "sync-busy job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	remoteDir := workingDir
	scope := sourceSyncLeaseScope("test-host", remoteDir)
	acquired, err := db.AcquireAutoLease(database, scope, "other-source-sync", sourceSyncLeaseTTL)
	if err != nil || !acquired {
		t.Fatalf("seed source sync lease: acquired=%v err=%v", acquired, err)
	}

	syncCalls := 0
	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		syncCalls++
		t.Fatal("source sync should not run while another caller holds the lease")
		return nil
	}))

	appendCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return "__WEFT_NO_STATE_FILE__\n", "", 0
		case strings.Contains(command, `"op":"add"`):
			appendCount++
			t.Errorf("unexpected append while source sync is already in flight: %s", command)
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 {
		t.Errorf("ensured = %d, want 0", ensured)
	}
	if !contacted {
		t.Error("contacted = false, want true after the runner-state probe reached the host")
	}
	if syncCalls != 0 {
		t.Fatalf("syncCalls = %d, want 0", syncCalls)
	}
	if appendCount != 0 {
		t.Fatalf("appendCount = %d, want 0", appendCount)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.LastSyncedStatus != "" {
		t.Errorf("LastSyncedStatus = %q, want empty", job.LastSyncedStatus)
	}
}

func TestSourceSyncLeaseTTLTracksLongSourceTimeout(t *testing.T) {
	if got := sourceSyncLeaseTTLFor(5 * time.Second); got != sourceSyncLeaseTTL {
		t.Fatalf("short timeout TTL = %s, want default %s", got, sourceSyncLeaseTTL)
	}
	if got := sourceSyncLeaseTTLFor(10 * time.Minute); got != 11*time.Minute {
		t.Fatalf("long timeout TTL = %s, want 11m", got)
	}
}

// TestEffectiveSourceSyncTimeout guards against the regression where source
// rsync inherited the few-second SSH status-probe Timeout. The daemon host-sync
// worker passes a 5s Timeout and leaves SourceTimeout unset; before the fix that
// SIGKILL'd source rsync mid-connect, blocking the job with "source sync failed:
// ... timed out after 5s". Source sync must instead get the source-sync default.
func TestEffectiveSourceSyncTimeout(t *testing.T) {
	// Unset SourceTimeout must not fall back to the short status Timeout.
	got := effectiveSourceSyncTimeout(HostSyncOptions{Timeout: 5 * time.Second})
	if got != defaultSourceSyncTimeout {
		t.Fatalf("unset SourceTimeout with 5s Timeout = %s, want source default %s", got, defaultSourceSyncTimeout)
	}
	if got <= 5*time.Second {
		t.Fatalf("source sync budget %s is too short to survive SSH connect setup", got)
	}

	// An explicit SourceTimeout is honored verbatim (the syncorch path sets it).
	if got := effectiveSourceSyncTimeout(HostSyncOptions{Timeout: 5 * time.Second, SourceTimeout: 30 * time.Second}); got != 30*time.Second {
		t.Fatalf("explicit SourceTimeout = %s, want 30s", got)
	}
}

func TestSyncHost_SurfacesQueueDispatchFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "sync-fail job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return fmt.Errorf("rsync timeout")
	}))

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "", 0
	})

	result, err := SyncHost(database, "test-host", HostSyncOptions{Timeout: time.Second}, nil)
	if err != nil {
		t.Fatalf("SyncHost: %v", err)
	}
	if result.QueueDispatchError == "" {
		t.Fatal("expected QueueDispatchError to be populated")
	}
	if !strings.Contains(result.QueueDispatchError, fmt.Sprintf("job %s source sync failed", ids.FormatJobID(jobID))) {
		t.Fatalf("QueueDispatchError = %q, want job-specific source sync failure", result.QueueDispatchError)
	}
	if result.Updated != 0 {
		t.Fatalf("Updated = %d, want 0", result.Updated)
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsAlreadySynced(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "synced job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Mark as already synced
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	// No SSH mock needed — should skip without making SSH calls
	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (already synced), got %d", ensured)
	}
	if contacted {
		t.Error("expected contacted=false for already synced")
	}
}

func TestEnsureQueuedJobsOnRemote_RedispatchesSyncedQueuedJobWithMissingPayload(t *testing.T) {
	database := db.SetupTestDB(t)

	workingDir := t.TempDir()
	jobID, err := db.RecordQueued(database, "test-host", workingDir, "echo hello", "synced missing payload")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobBackend(database, jobID, db.BackendQueueRunner); err != nil {
		t.Fatalf("set backend: %v", err)
	}
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return nil
	}))

	appendCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		switch {
		case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
			return fmt.Sprintf(`{"agent_version":"test-agent","queue_protocol_version":1,"pending":[%d],"current":null}`, jobID) + "\n", "", 0
		case strings.Contains(command, "job-${id}.json"):
			return fmt.Sprintf("%d\tMISSING\n", jobID), "", 0
		case strings.Contains(command, `"op":"add"`):
			appendCount++
			if !strings.Contains(command, fmt.Sprintf(`"id":%d`, jobID)) {
				t.Fatalf("append command missing job id: %s", command)
			}
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 1 {
		t.Fatalf("ensured = %d, want 1", ensured)
	}
	if !contacted {
		t.Fatal("contacted = false, want true")
	}
	if appendCount != 1 {
		t.Fatalf("appendCount = %d, want 1", appendCount)
	}
}

// TestEnsureQueuedJobsOnRemote_DoesNotRedispatchDispatchStillInFlight is the
// regression for the studio re-dispatch loop (wj8973 took 16 identical add
// commands in 33 minutes for one attempt). Weft appends the add to the queue
// and the runner materializes the payload only when it consumes that command.
// Between those two moments the payload probe truthfully reports MISSING, and
// reading that as "the runner lost the job" resets the durable dispatch and
// re-appends it — forever, since every pass re-observes the same gap.
//
// The observable contract: while the runner's cursor is behind weft's last
// re-dispatch, no new add command is sent and last_synced_status survives; a
// deferral records the uncertainty. Once the cursor passes that instant the
// absence is real evidence and the job is re-dispatched.
func TestEnsureQueuedJobsOnRemote_DoesNotRedispatchDispatchStillInFlight(t *testing.T) {
	workingDir := t.TempDir()
	redispatchedAt := time.Now().Add(-2 * time.Minute)

	// cursorOffset shifts the runner's command cursor relative to weft's
	// recorded re-dispatch: behind it the dispatch is still in flight; past
	// it the runner has applied every command weft sent.
	run := func(t *testing.T, cursorOffset time.Duration) (appendCount, redispatchMarkers int, deferral string) {
		t.Helper()
		database := db.SetupTestDB(t)
		jobID, err := db.RecordQueued(database, "test-host", workingDir, "echo hello", "in-flight dispatch")
		if err != nil {
			t.Fatalf("record queued job: %v", err)
		}
		if err := db.SetJobBackend(database, jobID, db.BackendQueueRunner); err != nil {
			t.Fatalf("set backend: %v", err)
		}
		if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
			t.Fatalf("update last synced status: %v", err)
		}
		// Weft already reset and re-appended this job two minutes ago.
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			EventKind:  db.EventQueueDispatchDeferred,
			JobID:      jobID,
			OccurredAt: redispatchedAt.Unix(),
			Detail:     redispatchPublicationMissingDetail,
		}); err != nil {
			t.Fatalf("record prior re-dispatch: %v", err)
		}

		t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
			return nil
		}))
		cursor := redispatchedAt.Add(cursorOffset).UTC().Format(time.RFC3339Nano)
		mockSSHFunc(t, func(host, command string) (string, string, int) {
			switch {
			case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
				return fmt.Sprintf(`{"agent_version":"test-agent","queue_protocol_version":1,"cursor":%q,"pending":[%d],"current":null}`, cursor, jobID) + "\n", "", 0
			case strings.Contains(command, "job-${id}.json"):
				// The add command has not been consumed yet, so no payload
				// file exists for it.
				return fmt.Sprintf("%d\tMISSING\n", jobID), "", 0
			case strings.Contains(command, `"op":"add"`):
				appendCount++
				return "", "", 0
			default:
				return "", "", 0
			}
		})

		if _, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default()); err != nil {
			t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
		}
		events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
			JobID: jobID, Kind: db.EventQueueDispatchDeferred, Limit: 10,
		})
		if err != nil {
			t.Fatalf("list deferrals: %v", err)
		}
		for _, event := range events {
			if event.Detail == redispatchPublicationMissingDetail {
				redispatchMarkers++
			} else if deferral == "" {
				deferral = event.Detail
			}
		}
		return appendCount, redispatchMarkers, deferral
	}

	t.Run("runner has not consumed the previous dispatch", func(t *testing.T) {
		appendCount, redispatchMarkers, deferral := run(t, -30*time.Second)
		if appendCount != 0 {
			t.Errorf("appendCount = %d, want 0: weft re-dispatched a job whose add the runner has not read yet", appendCount)
		}
		if redispatchMarkers != 1 {
			t.Errorf("recorded re-dispatches = %d, want 1 (only the pre-existing one): a dispatch still in flight was reset and re-appended", redispatchMarkers)
		}
		if !strings.Contains(deferral, "has not consumed queue commands") {
			t.Errorf("deferral detail = %q, want the unconsumed-dispatch explanation", deferral)
		}
	})

	t.Run("runner consumed it and still lacks the job", func(t *testing.T) {
		appendCount, redispatchMarkers, _ := run(t, time.Second)
		if appendCount != 1 {
			t.Errorf("appendCount = %d, want 1: a genuinely lost job was not recovered", appendCount)
		}
		if redispatchMarkers != 2 {
			t.Errorf("recorded re-dispatches = %d, want 2: the recovery was not recorded", redispatchMarkers)
		}
	})
}

func TestEnsureQueuedJobsOnRemote_SkipsPendingStatus(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "pending job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Set a pending status (e.g., pending cancel)
	cancelStatus := db.StatusCanceled
	if err := db.SetPendingStatus(database, jobID, cancelStatus); err != nil {
		t.Fatalf("set pending status: %v", err)
	}

	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (has pending status), got %d", ensured)
	}
	if contacted {
		t.Error("expected contacted=false for pending status")
	}
}

func TestProcessDeferredQueueOps_RemoveQueued(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "queued job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.AddDeferredOperation(database, "test-host", db.OpRemoveQueued, jobID, ""); err != nil {
		t.Fatalf("add deferred op: %v", err)
	}
	if err := db.MoveQueuedJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("MoveQueuedJobToUnplaced: %v", err)
	}

	callCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		callCount++
		return "", "", 0
	})

	result, err := ProcessDeferredQueueOps(database, "test-host", 5*time.Second)
	if err != nil {
		t.Fatalf("ProcessDeferredQueueOps: %v", err)
	}
	if !result.HostContacted {
		t.Error("expected HostContacted to be true")
	}
	if callCount == 0 {
		t.Error("expected remote cleanup SSH command")
	}
	pending, err := db.HasPendingOperation(database, jobID, db.OpRemoveQueued)
	if err != nil {
		t.Fatalf("HasPendingOperation: %v", err)
	}
	if pending {
		t.Error("expected remove_queued deferred op to be cleared")
	}
}

func TestEnsureHFInputsAvailable_DownloadsMissingHFAsset(t *testing.T) {
	database := db.SetupTestDB(t)
	scanCount := 0

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if host != "test-host" {
			t.Fatalf("unexpected host %q", host)
		}
		switch {
		case strings.Contains(command, "df -Pk"):
			return "20971520\n", "", 0
		// Daemonized download path (runDetachedRemoteCommand in
		// internal/dataloc/download.go): the spawn issues a `nohup ...
		// cmd.sh ...` invocation; each poll calls
		// `if [ -f "$D/status" ]`. The test cares about the request
		// completing, not what the download actually did — return OK to
		// spawn, STATUS=0 to the first poll.
		case strings.Contains(command, "nohup") && strings.Contains(command, "cmd.sh"):
			return "OK\n", "", 0
		case strings.Contains(command, `if [ -f "$D/status" ]`):
			return "STATUS=0\n---STDERR---\n", "", 0
		case strings.Contains(command, "$_hfdl download --repo-type model"):
			return "", "", 0
		case strings.Contains(command, "du -sb"), strings.Contains(command, "ls -1d"):
			scanCount++
			if scanCount >= 2 {
				return "2048\tok\t/home/test/.cache/huggingface/hub/models--bert-base-uncased\n", "", 0
			}
			return "", "", 0
		default:
			return "", "", 0
		}
	})
	t.Cleanup(dataloc.SetDetachedPollIntervalForTest(time.Millisecond))

	if _, err := ensureHFInputsAvailable(database, "test-host", []string{"hf:bert-base-uncased"}, nil, 5*time.Second); err != nil {
		t.Fatalf("ensureHFInputsAvailable: %v", err)
	}

	entries, err := dataloc.FindAssetHosts(database, dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "bert-base-uncased"})
	if err != nil {
		t.Fatalf("FindAssetHosts: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Host != "test-host" {
		t.Fatalf("host = %q, want test-host", entries[0].Host)
	}
}

func TestEnsureHFInputsAvailable_FallsBackAfterDonorConnectionFailure(t *testing.T) {
	database := db.SetupTestDB(t)
	t.Cleanup(inventory.SetHosts([]inventory.HostSpec{{
		Name: "target-host", HFCacheDir: "/home/test/.cache/huggingface/hub",
	}}))
	asset := dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "bert-base-uncased"}
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host: "donor-host", Asset: asset,
		Path: "/donor/cache/hub/models--bert-base-uncased", SizeBytes: 2048,
		LastSeen: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	scanCount := 0
	donorCalls := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if host == "donor-host" {
			donorCalls++
			return "", "ssh: connect to host target-host port 22: Operation timed out", 255
		}
		if host != "target-host" {
			t.Fatalf("unexpected host %q", host)
		}
		switch {
		case strings.Contains(command, "df -Pk"):
			return "20971520\n", "", 0
		case strings.Contains(command, "nohup") && strings.Contains(command, "cmd.sh"):
			return "OK\n", "", 0
		case strings.Contains(command, `if [ -f "$D/status" ]`):
			return "STATUS=0\n---STDERR---\n", "", 0
		case strings.Contains(command, "du -sb"), strings.Contains(command, "ls -1d"):
			scanCount++
			if scanCount >= 2 {
				return "2048\tok\t/home/test/.cache/huggingface/hub/models--bert-base-uncased\n", "", 0
			}
			return "", "", 0
		default:
			return "", "", 0
		}
	})
	t.Cleanup(dataloc.SetDetachedPollIntervalForTest(time.Millisecond))

	if _, err := ensureHFInputsAvailable(database, "target-host", []string{"hf:bert-base-uncased"}, nil, 5*time.Second); err != nil {
		t.Fatalf("ensureHFInputsAvailable: %v", err)
	}
	if donorCalls == 0 {
		t.Fatal("prestage donor was not attempted")
	}
	entries, err := dataloc.FindAssetHosts(database, asset)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(entries, func(entry dataloc.HostDataEntry) bool {
		return entry.Host == "target-host"
	}) {
		t.Fatalf("target asset entry missing after fallback: %+v", entries)
	}
}

func TestHFInputStageTimeout(t *testing.T) {
	if got := hfInputStageTimeout(0); got != minHFInputStageTimeout {
		t.Fatalf("hfInputStageTimeout(0) = %s, want %s", got, minHFInputStageTimeout)
	}
	if got := hfInputStageTimeout(30 * time.Second); got != minHFInputStageTimeout {
		t.Fatalf("hfInputStageTimeout(30s) = %s, want %s", got, minHFInputStageTimeout)
	}
	longTimeout := 15 * time.Minute
	if got := hfInputStageTimeout(longTimeout); got != longTimeout {
		t.Fatalf("hfInputStageTimeout(15m) = %s, want %s", got, longTimeout)
	}
}

// TestStageMissingNeeds_LeaseSkipsConcurrentDuplicate verifies that when a
// staging lease for the same (host, artifact) is already held, a second
// caller skips the transfer instead of spawning a duplicate scp. This is
// the regression for overlapping sync passes stomping the same .weft-staging
// destination.
func TestStageMissingNeeds_LeaseSkipsConcurrentDuplicate(t *testing.T) {
	database := db.SetupTestDB(t)

	producerID, err := db.RecordQueued(database, "", "/tmp", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}
	consumerID, err := db.RecordQueued(database, "host-alpha", "/tmp", "consume", "consumer")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	if err := db.SetJobNeeds(database, consumerID, []string{fmt.Sprintf("output/big.pt:%d", producerID)}); err != nil {
		t.Fatalf("set needs: %v", err)
	}
	consumer, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get consumer: %v", err)
	}

	pending, err := collectPendingNeeds(database, consumer)
	if err != nil {
		t.Fatalf("collectPendingNeeds: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending need, got %d", len(pending))
	}

	// Pre-acquire the lease as if another caller is already mid-transfer.
	scope := needsStageLeaseScope("host-alpha", pending[0].markerName)
	acquired, err := db.AcquireAutoLease(database, scope, "other-caller", needsStageLeaseTTL)
	if err != nil || !acquired {
		t.Fatalf("seed lease: acquired=%v err=%v", acquired, err)
	}

	// getR2Client must NOT be called when the transfer is skipped.
	getR2 := func() (*r2.Client, error) {
		t.Fatal("getR2Client should not be called when lease is held by another caller")
		return nil, nil
	}

	probeState := map[string]remoteNeedState{
		pending[0].markerName: {markerExists: false, fileSize: -1, stagingSize: -1},
	}
	if err := stageMissingNeeds(database, consumer, pending, probeState, time.Second, getR2); err != nil {
		t.Fatalf("stageMissingNeeds: %v", err)
	}
}

func TestFindExistingScpToRemote(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not on PATH; skipping")
	}

	// Start a long-running placeholder process whose command line will
	// match the regex pgrep expects. It needs to look enough like a
	// real scp invocation to satisfy the `scp .*<host>:<path>` pattern.
	// `sleep` with a tagged comment in argv works on macOS and Linux.
	const remoteHost = "test-fake-host-9f3a"
	const remotePath = "/tmp/weft-pid-check-fixture/file.bin"
	cmd := exec.Command("sh", "-c", fmt.Sprintf("exec -a 'scp -q /tmp/x %s:%s.weft-staging' sleep 30", remoteHost, remotePath))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start placeholder: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// pgrep argv visibility on macOS sometimes lags briefly after exec;
	// poll a couple times.
	var found []int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		found = findExistingScpToRemote(remoteHost, remotePath+".weft-staging")
		if len(found) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(found) == 0 {
		t.Skip("pgrep didn't match the placeholder argv (platform shell limitation); fixture-side issue, not a code regression")
	}

	// Negative case: a different remote host should not match.
	if pids := findExistingScpToRemote("totally-different-host", remotePath+".weft-staging"); len(pids) > 0 {
		t.Errorf("findExistingScpToRemote returned %v for unrelated host", pids)
	}
}

// An input inferred from the command line (best-effort) that can never be
// staged — here an agent harness's `--model provider/model` selector that
// has the shape of an HF repo id — must not hold its job back: the job is
// dispatched without it, as the cloud agent does. A declared input with the
// same failure still blocks dispatch (wb173).
func TestEnsureQueuedJobsOnRemote_BestEffortInputStagingFailureDoesNotBlockDispatch(t *testing.T) {
	const input = "hf:zhipu-coding-plan/glm-5.3-flash"
	cases := []struct {
		name         string
		bestEffort   bool
		wantEnsured  int
		wantAppended int
	}{
		{name: "inferred input is skipped", bestEffort: true, wantEnsured: 1, wantAppended: 1},
		{name: "declared input blocks", bestEffort: false, wantEnsured: 0, wantAppended: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := db.SetupTestDB(t)
			jobID, err := db.RecordQueued(database, "test-host", "", "omp --model zhipu-coding-plan/glm-5.3-flash --print hi", "agent job")
			if err != nil {
				t.Fatalf("record queued job: %v", err)
			}
			if err := db.SetJobInputs(database, jobID, []string{input}); err != nil {
				t.Fatalf("set inputs: %v", err)
			}
			if tc.bestEffort {
				if err := db.SetJobMetadata(database, jobID, &db.JobMetadata{BestEffortInputs: []string{input}}); err != nil {
					t.Fatalf("set metadata: %v", err)
				}
			}

			appended := 0
			mockSSHFunc(t, func(host, command string) (string, string, int) {
				switch {
				case strings.Contains(command, "__WEFT_NO_STATE_FILE__"):
					return currentRunnerStateJSON + "\n", "", 0
				case strings.Contains(command, `"op":"add"`):
					appended++
					return "", "", 0
				case strings.Contains(command, "df -Pk"):
					return "20971520\n", "", 0
				case strings.Contains(command, "nohup") && strings.Contains(command, "cmd.sh"):
					return "OK\n", "", 0
				case strings.Contains(command, `if [ -f "$D/status" ]`):
					return "STATUS=1\n---STDERR---\nError: Model 'zhipu-coding-plan/glm-5.3-flash' not found.\n", "", 0
				default:
					return "", "", 0
				}
			})
			t.Cleanup(dataloc.SetDetachedPollIntervalForTest(time.Millisecond))

			ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, 5*time.Second, slog.Default())
			if ensured != tc.wantEnsured {
				t.Fatalf("ensured = %d (err %v), want %d", ensured, err, tc.wantEnsured)
			}
			if appended != tc.wantAppended {
				t.Fatalf("queue appends = %d, want %d", appended, tc.wantAppended)
			}
			if tc.bestEffort && err != nil {
				t.Fatalf("unexpected dispatch error for a best-effort input: %v", err)
			}
			if !tc.bestEffort && (err == nil || !strings.Contains(err.Error(), "input staging failed")) {
				t.Fatalf("err = %v, want input staging failure for a declared input", err)
			}
		})
	}
}
