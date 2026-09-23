package ops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventoryqueue"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2resolve"
)

func setupR2QueueNeeds(t *testing.T) (*sql.DB, *fakeInventoryQueueStore, func([]string, ...int)) {
	t.Helper()
	database := db.SetupTestDB(t)
	oldLoad, oldStore, oldSSH := loadQueueConfig, newInventoryQueueStore, appendQueueCommandSSH
	oldExists, oldRuns, oldList := r2resolve.ObjectExistsFunc, r2resolve.ListRunIDsFunc, r2resolve.ListObjectsFunc
	oldReportFetch := getAttemptPublicationReport
	t.Cleanup(func() {
		loadQueueConfig, newInventoryQueueStore, appendQueueCommandSSH = oldLoad, oldStore, oldSSH
		r2resolve.ObjectExistsFunc, r2resolve.ListRunIDsFunc, r2resolve.ListObjectsFunc = oldExists, oldRuns, oldList
		getAttemptPublicationReport = oldReportFetch
	})
	loadQueueConfig = func() (*config.Config, error) {
		return &config.Config{Hosts: map[string]config.HostConfig{
			"host-alpha": {QueueTransport: queueTransportR2Pull},
		}}, nil
	}
	store := &fakeInventoryQueueStore{objects: map[string][]byte{}}
	newInventoryQueueStore = func() (inventoryQueueStore, error) { return store, nil }
	appendQueueCommandSSH = func(string, opsqueue.QueueCommand, opsqueue.AppendCommandOptions) error {
		t.Fatal("R2-pull queue attempted SSH publication")
		return nil
	}
	mockSSHFunc(t, func(string, string) (string, string, int) {
		t.Fatal("R2-pull dispatch attempted SSH")
		return "", "", 1
	})
	r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, key string) (bool, error) {
		_, exists := store.objects[key]
		return exists, nil
	}
	r2resolve.ListObjectsFunc = func(_ context.Context, _ r2resolve.Lister, prefix string) ([]r2.ObjectInfo, error) {
		var objects []r2.ObjectInfo
		for key := range store.objects {
			if strings.HasPrefix(key, prefix) {
				objects = append(objects, r2.ObjectInfo{Key: key})
			}
		}
		return objects, nil
	}
	r2resolve.ListRunIDsFunc = func(context.Context, r2resolve.Lister, int64) ([]int64, error) {
		return nil, nil
	}
	// No publication report on R2 unless a test publishes one: the resolver
	// then falls back to the stored row, matching pre-refresh behavior.
	getAttemptPublicationReport = func(context.Context, *r2.Client, string) ([]byte, error) {
		return nil, nil
	}
	// artifactNeedVersion defaults to a current agent. A caller passing an
	// explicit version models an agent that has not been redeployed, which the
	// capability string alone cannot distinguish from a current one.
	publishState := func(capabilities []string, artifactNeedVersion ...int) {
		t.Helper()
		version := opsqueue.ArtifactNeedVersionProducerArtifacts
		if len(artifactNeedVersion) > 0 {
			version = artifactNeedVersion[0]
		}
		key, err := inventoryqueue.StateKey("host-alpha")
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(inventoryqueue.State{
			Version: inventoryqueue.Version, Host: "host-alpha", UpdatedAt: time.Now(),
			Runner: opsqueue.RunnerState{
				QueueProtocolVersion: opsqueue.QueueProtocolVersion,
				Capabilities:         capabilities,
				ArtifactNeedVersion:  version,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		store.objects[key] = data
	}
	publishState([]string{opsqueue.CapabilityArtifactNeedV1})
	return database, store, publishState
}

func recordR2NeedsConsumer(t *testing.T, database *sql.DB, needs []string) *db.Job {
	t.Helper()
	id, err := RecordQueuedJob(database, QueueJobParams{
		Host: "host-alpha", WorkingDir: "/tmp/consumer", Command: "cat outputs/result.bin", Needs: needs,
		Metadata: &db.JobMetadata{Source: &db.JobSourceMetadata{Pin: &db.JobSourcePinMetadata{
			Hash:  strings.Repeat("a", 64),
			Roots: []db.JobSourcePinRootMetadata{{MountBasename: "consumer", Hash: strings.Repeat("b", 64), R2Key: "sources/root.tar.gz"}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobBackend(database, id, db.BackendQueueRunner); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func recordNeedProducerWithoutPublication(t *testing.T, database *sql.DB, host string) (string, string, int64) {
	t.Helper()
	id, err := db.RecordQueued(database, host, "/tmp/producer", "produce", "producer")
	if err != nil {
		t.Fatal(err)
	}
	if host == "" {
		launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateAttempt(database, id, "", &launchID, db.StatusRunning); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, exit_code = 0, end_time = ? WHERE job_id = ?`, db.StatusCompleted, time.Now().Unix(), id); err != nil {
		t.Fatal(err)
	}
	producer, err := db.GetJobByID(database, id)
	if err != nil || producer.LatestRunID == nil {
		t.Fatalf("producer attempt: %+v, %v", producer, err)
	}
	runID := *producer.LatestRunID
	return fmt.Sprintf("outputs/result.bin:%d", id), fmt.Sprintf("jobs/%d/runs/%d/outputs/outputs/result.bin", id, runID), runID
}

func publishNeedProducer(t *testing.T, database *sql.DB, spec, key string, runID int64) {
	t.Helper()
	var jobID int64
	if _, err := fmt.Sscanf(spec, "outputs/result.bin:%d", &jobID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.UpsertAttemptPublicationState(database, &db.AttemptPublicationState{
		AttemptID: runID, JobID: jobID, Sequence: 1, ObservedAt: now,
		ExecutionState: db.PublicationExecutionComplete, ExecutionCompletedAt: &now,
		RequiredArtifactsState: db.PublicationStateReady, RequiredArtifactsReadyAt: &now,
		DrainState: db.PublicationStateReady, DrainCompletedAt: &now,
		Artifacts: []db.AttemptPublicationArtifact{{
			Name: "result", Path: "outputs/result.bin", State: db.PublicationStateReady,
			ReadyAt: &now, PayloadKey: key,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	store, err := newInventoryQueueStore()
	if err != nil {
		t.Fatal(err)
	}
	store.(*fakeInventoryQueueStore).objects[key] = []byte("published producer bytes")
}

func recordNeedProducer(t *testing.T, database *sql.DB, host string) (string, string) {
	t.Helper()
	spec, key, runID := recordNeedProducerWithoutPublication(t, database, host)
	publishNeedProducer(t, database, spec, key, runID)
	return spec, key
}

func r2QueueAdds(t *testing.T, store *fakeInventoryQueueStore) []opsqueue.CommandJob {
	t.Helper()
	var jobs []opsqueue.CommandJob
	for key, data := range store.objects {
		if !strings.Contains(key, "/inbox/") {
			continue
		}
		var request inventoryqueue.Request
		if err := json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		if request.Command.Op == opsqueue.OpAdd && request.Command.Job != nil {
			jobs = append(jobs, *request.Command.Job)
		}
	}
	return jobs
}

func TestR2QueueArtifactNeeds(t *testing.T) {
	for _, kind := range []string{"producer", "inventory-producer", "named", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			database, store, _ := setupR2QueueNeeds(t)
			var specs []string
			var want []opsqueue.ArtifactNeed
			if kind != "named" {
				host := ""
				if kind == "inventory-producer" {
					host = "host-beta"
				}
				spec, key := recordNeedProducer(t, database, host)
				specs = append(specs, spec)
				want = append(want, opsqueue.ArtifactNeed{Spec: spec, Path: "outputs/result.bin", R2Key: key})
			}
			if kind == "named" || kind == "mixed" {
				if err := db.UpsertNamedAsset(database, db.NamedAsset{Name: "trace", ContentHash: "trace-hash", ContentType: "directory", TargetPath: "data/trace"}); err != nil {
					t.Fatal(err)
				}
				specs = append(specs, "asset:trace")
				want = append(want, opsqueue.ArtifactNeed{Spec: "asset:trace", Path: "data/trace", R2Key: "assets/trace-hash", ContentType: "directory"})
			}
			job := recordR2NeedsConsumer(t, database, specs)
			if err := AppendJobToQueue(database, job, time.Second); err != nil {
				t.Fatal(err)
			}
			// Edits must not erase the staging metadata that initial dispatch sent.
			if err := applyQueueUpdate(database, job, nil, job.DepSpec, time.Second); err != nil {
				t.Fatal(err)
			}
			adds := r2QueueAdds(t, store)
			if len(adds) != 2 {
				t.Fatalf("queue publications = %d, want initial add and update", len(adds))
			}
			for _, sent := range adds {
				if !reflect.DeepEqual(sent.ArtifactNeeds, want) {
					t.Fatalf("artifact needs = %+v, want %+v", sent.ArtifactNeeds, want)
				}
			}
		})
	}
}

func TestR2QueueNeedsRequireCapability(t *testing.T) {
	for _, kind := range []string{"producer", "named"} {
		t.Run(kind, func(t *testing.T) {
			database, store, publishState := setupR2QueueNeeds(t)
			publishState(nil)
			spec := "asset:trace"
			if kind == "producer" {
				spec, _ = recordNeedProducer(t, database, "")
			} else if err := db.UpsertNamedAsset(database, db.NamedAsset{Name: "trace", ContentHash: "hash", ContentType: "file", TargetPath: "data/trace"}); err != nil {
				t.Fatal(err)
			}
			job := recordR2NeedsConsumer(t, database, []string{spec})
			err := AppendJobToQueue(database, job, time.Second)
			if err == nil || !opsqueue.IsMissingRunnerCapabilityBlock(err.Error()) {
				t.Fatalf("want capability refusal, got %v", err)
			}
			if adds := r2QueueAdds(t, store); len(adds) != 0 {
				t.Fatalf("incapable runner received jobs: %+v", adds)
			}
		})
	}
}

func TestR2QueueProducerNeedRejectsMismatchedAttemptKey(t *testing.T) {
	database, store, _ := setupR2QueueNeeds(t)
	spec, _, runID := recordNeedProducerWithoutPublication(t, database, "")
	var producerID int64
	if _, err := fmt.Sscanf(spec, "outputs/result.bin:%d", &producerID); err != nil {
		t.Fatal(err)
	}
	staleKey := fmt.Sprintf("jobs/%d/runs/%d/outputs/outputs/result.bin", producerID, runID+1)
	publishNeedProducer(t, database, spec, staleKey, runID)
	job := recordR2NeedsConsumer(t, database, []string{spec})

	err := AppendJobToQueue(database, job, time.Second)
	if err == nil || !strings.Contains(err.Error(), "mismatched payload key") {
		t.Fatalf("want attempt-provenance refusal, got %v", err)
	}
	if adds := r2QueueAdds(t, store); len(adds) != 0 {
		t.Fatalf("mismatched attempt published queue entries: %+v", adds)
	}
}

func attemptPublicationReportPayload(payloadKey string) []byte {
	return []byte(fmt.Sprintf(`{"sequence":2,"observed_at_unix":1700000000,`+
		`"facets":{"execution_state":"complete","required_artifacts_state":"ready","drain_state":"ready"},`+
		`"artifacts":[{"name":"result","path":"outputs/result.bin","state":"ready",`+
		`"ready_at_unix":1700000000,"payload_key":%q}]}`, payloadKey))
}

func stubAttemptPublicationReport(t *testing.T, jobID, runID int64, report []byte) {
	t.Helper()
	reportKey := r2keys.JobAttemptPublicationReport(jobID, runID)
	getAttemptPublicationReport = func(_ context.Context, _ *r2.Client, gotKey string) ([]byte, error) {
		if gotKey != reportKey {
			t.Errorf("publication report key = %q, want exact-attempt %q", gotKey, reportKey)
			return nil, nil
		}
		return report, nil
	}
}

// A completed producer whose stored publication row is absent or stale must
// still unblock dispatch when the authoritative exact-attempt report on R2
// is published: the resolver fetches and ingests that report instead of
// refusing on the stale row.
func TestR2QueueProducerNeedRefreshesPublicationFromAttemptReport(t *testing.T) {
	for _, stored := range []string{"absent", "pending"} {
		t.Run(stored, func(t *testing.T) {
			database, store, _ := setupR2QueueNeeds(t)
			spec, key, runID := recordNeedProducerWithoutPublication(t, database, "")
			var producerID int64
			if _, err := fmt.Sscanf(spec, "outputs/result.bin:%d", &producerID); err != nil {
				t.Fatal(err)
			}
			if stored == "pending" {
				now := time.Now().Unix()
				if _, err := db.UpsertAttemptPublicationState(database, &db.AttemptPublicationState{
					AttemptID: runID, JobID: producerID, Sequence: 1, ObservedAt: now,
					ExecutionState:         db.PublicationExecutionPending,
					RequiredArtifactsState: db.PublicationStatePending,
					DrainState:             db.PublicationStatePending,
					Artifacts: []db.AttemptPublicationArtifact{{
						Name: "result", Path: "outputs/result.bin", State: db.PublicationStatePending,
					}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			stubAttemptPublicationReport(t, producerID, runID, attemptPublicationReportPayload(key))
			store.objects[key] = []byte("published producer bytes")
			job := recordR2NeedsConsumer(t, database, []string{spec})

			if err := AppendJobToQueue(database, job, time.Second); err != nil {
				t.Fatalf("dispatch with published exact-attempt report (%s stored row): %v", stored, err)
			}
			adds := r2QueueAdds(t, store)
			if len(adds) != 1 || len(adds[0].ArtifactNeeds) != 1 || adds[0].ArtifactNeeds[0].R2Key != key {
				t.Fatalf("published attempt report did not dispatch exact key: %+v", adds)
			}
			// The fetched report is ingested, so later passes decide from the
			// stored row without another R2 round-trip.
			state, err := db.GetAttemptPublicationState(database, runID)
			if err != nil {
				t.Fatal(err)
			}
			if state == nil || state.ExecutionState != db.PublicationExecutionComplete ||
				len(state.Artifacts) != 1 || state.Artifacts[0].State != db.PublicationStateReady ||
				state.Artifacts[0].PayloadKey != key {
				t.Fatalf("attempt report not ingested: %+v", state)
			}
		})
	}
}

// The mismatched-key refusal applies to a refreshed report exactly as it
// does to a stored row: bytes left by a different attempt are not
// admissible evidence for this one.
func TestR2QueueProducerNeedRefreshedReportRejectsMismatchedKey(t *testing.T) {
	database, store, _ := setupR2QueueNeeds(t)
	spec, _, runID := recordNeedProducerWithoutPublication(t, database, "")
	var producerID int64
	if _, err := fmt.Sscanf(spec, "outputs/result.bin:%d", &producerID); err != nil {
		t.Fatal(err)
	}
	staleKey := fmt.Sprintf("jobs/%d/runs/%d/outputs/outputs/result.bin", producerID, runID+1)
	stubAttemptPublicationReport(t, producerID, runID, attemptPublicationReportPayload(staleKey))
	job := recordR2NeedsConsumer(t, database, []string{spec})

	err := AppendJobToQueue(database, job, time.Second)
	if err == nil || !strings.Contains(err.Error(), "mismatched payload key") {
		t.Fatalf("want attempt-provenance refusal from refreshed report, got %v", err)
	}
	if adds := r2QueueAdds(t, store); len(adds) != 0 {
		t.Fatalf("mismatched attempt published queue entries: %+v", adds)
	}
}

// A ready artifact without a payload key is resolved by probing R2 for the
// exact-attempt object. That probe runs unbounded on context.Background
// before the wb137 review repair; it must carry the same 20s bound as the
// r2resolve object-existence helper.
func TestR2QueueProducerNeedReadyProbeUsesBoundedContext(t *testing.T) {
	database, store, _ := setupR2QueueNeeds(t)
	spec, _, runID := recordNeedProducerWithoutPublication(t, database, "")
	var producerID int64
	if _, err := fmt.Sscanf(spec, "outputs/result.bin:%d", &producerID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.UpsertAttemptPublicationState(database, &db.AttemptPublicationState{
		AttemptID: runID, JobID: producerID, Sequence: 1, ObservedAt: now,
		ExecutionState: db.PublicationExecutionComplete, ExecutionCompletedAt: &now,
		RequiredArtifactsState: db.PublicationStateReady, RequiredArtifactsReadyAt: &now,
		DrainState: db.PublicationStateReady, DrainCompletedAt: &now,
		Artifacts: []db.AttemptPublicationArtifact{{
			Name: "result", Path: "outputs/result.bin", State: db.PublicationStateReady, ReadyAt: &now,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var probedKey string
	r2resolve.ObjectExistsFunc = func(ctx context.Context, _ r2resolve.Store, key string) (bool, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("object-existence probe ran without a deadline")
		} else if remaining := time.Until(deadline); remaining <= 0 || remaining > producerR2ProbeTimeout {
			t.Errorf("object-existence probe deadline in %v, want within %v", remaining, producerR2ProbeTimeout)
		}
		probedKey = key
		return true, nil
	}
	job := recordR2NeedsConsumer(t, database, []string{spec})

	if err := AppendJobToQueue(database, job, time.Second); err != nil {
		t.Fatalf("dispatch with ready publication needing R2 probe: %v", err)
	}
	adds := r2QueueAdds(t, store)
	if len(adds) != 1 || len(adds[0].ArtifactNeeds) != 1 || adds[0].ArtifactNeeds[0].R2Key != probedKey {
		t.Fatalf("ready probe did not dispatch the probed exact-attempt key: %+v (probed %q)", adds, probedKey)
	}
}

func TestR2QueueNeedsLazyClient(t *testing.T) {
	database, store, _ := setupR2QueueNeeds(t)
	if err := db.UpsertNamedAsset(database, db.NamedAsset{Name: "trace", ContentHash: "hash", ContentType: "file", TargetPath: "data/trace"}); err != nil {
		t.Fatal(err)
	}
	for _, needs := range [][]string{nil, {"asset:trace"}} {
		job := recordR2NeedsConsumer(t, database, needs)
		err := appendJobToQueueWithSourceManifest(database, job, time.Second, "", "", nil,
			&opsqueue.RunnerState{QueueProtocolVersion: opsqueue.QueueProtocolVersion, Capabilities: []string{opsqueue.CapabilityArtifactNeedV1}},
			func() (*r2.Client, error) {
				t.Fatal("job without producer needs initialized R2 resolver client")
				return nil, nil
			})
		if err != nil {
			t.Fatal(err)
		}
	}
	if adds := r2QueueAdds(t, store); len(adds) != 2 {
		t.Fatalf("published %d jobs, want 2", len(adds))
	}
}

func TestR2QueueProducerDispatchWaitsForAttemptPublication(t *testing.T) {
	for _, route := range []string{"host-sync", "reconcile"} {
		t.Run(route, func(t *testing.T) {
			database, store, _ := setupR2QueueNeeds(t)
			spec, key, runID := recordNeedProducerWithoutPublication(t, database, "")
			job := recordR2NeedsConsumer(t, database, []string{spec})
			// Source transfer is independent of artifact resolution.
			oldSource := queueSourceSync
			t.Cleanup(func() { queueSourceSync = oldSource })
			queueSourceSync = func(*db.Job, time.Duration) (string, error) { return "", nil }
			dispatch := func() error {
				if route == "reconcile" {
					return applyQueueToRemote(database, job, time.Second)
				}
				_, _, err := ensureQueuedJobsOnRemote(database, job.Host, time.Second, time.Second, slog.Default())
				return err
			}

			err := dispatch()
			if err == nil || !strings.Contains(err.Error(), "publication unavailable") ||
				!strings.Contains(err.Error(), "outputs/result.bin") || !strings.Contains(err.Error(), "wj1") {
				t.Fatalf("want producer-specific unavailable-publication error, got %v", err)
			}
			if adds := r2QueueAdds(t, store); len(adds) != 0 {
				t.Fatalf("unpublished artifact published a queue entry: %+v", adds)
			}
			pending, err := db.GetJobByID(database, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if pending.Status != db.StatusQueued || pending.LastSyncedStatus != "" ||
				pending.FailureReason != "" || pending.ErrorMessage != "" || pending.ExitCode != nil || pending.EndTime != nil {
				t.Fatalf("unavailable publication became terminal or dispatched: %+v", pending)
			}
			if route == "host-sync" {
				var failures int
				if err := database.QueryRow(`SELECT COUNT(*) FROM lifecycle_events WHERE job_id = ? AND event_kind = ?`, job.ID, db.EventQueueDispatchFailed).Scan(&failures); err != nil {
					t.Fatal(err)
				}
				if failures != 1 {
					t.Fatalf("dispatch failures = %d, want one retry/backoff anchor", failures)
				}
			}

			publishNeedProducer(t, database, spec, key, runID)
			if err := dispatch(); err != nil {
				t.Fatalf("dispatch after attempt publication: %v", err)
			}
			adds := r2QueueAdds(t, store)
			if len(adds) != 1 || len(adds[0].ArtifactNeeds) != 1 || adds[0].ArtifactNeeds[0].R2Key != key {
				t.Fatalf("published producer attempt did not dispatch exact key: %+v", adds)
			}
		})
	}
}

func TestSSHProducerNeedsStillStageBeforeQueueAppend(t *testing.T) {
	database, _, _ := setupR2QueueNeeds(t)
	loadQueueConfig = func() (*config.Config, error) { return &config.Config{}, nil }
	spec, _ := recordNeedProducer(t, database, "")
	id, err := db.RecordQueued(database, "host-alpha", "", "consume", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobNeeds(database, id, []string{spec}); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, id)
	if err != nil {
		t.Fatal(err)
	}
	r2resolve.ObjectExistsFunc = func(context.Context, r2resolve.Store, string) (bool, error) { return false, nil }
	oldProbe := probeRemoteNeedsStateFunc
	t.Cleanup(func() { probeRemoteNeedsStateFunc = oldProbe })
	staged := false
	probeRemoteNeedsStateFunc = func(_ string, needs []pendingNeed, _ time.Duration) (map[string]remoteNeedState, error) {
		state := make(map[string]remoteNeedState)
		for _, need := range needs {
			state[need.markerName] = remoteNeedState{markerExists: staged, fileSize: -1, stagingSize: -1}
			if staged {
				state[need.markerName] = remoteNeedState{markerExists: true, fileSize: 8, stagingSize: -1}
			}
		}
		return state, nil
	}
	var sent *opsqueue.CommandJob
	appendQueueCommandSSH = func(_ string, command opsqueue.QueueCommand, _ opsqueue.AppendCommandOptions) error {
		sent = command.Job
		return nil
	}
	mockSSHFunc(t, func(string, string) (string, string, int) { return currentRunnerStateJSON + "\n", "", 0 })
	if err := applyQueueToRemote(database, job, time.Second); !errors.Is(err, r2resolve.ErrArtifactMissing) {
		t.Fatalf("SSH staging did not block missing producer bytes: %v", err)
	}
	if sent != nil {
		t.Fatal("SSH consumer appended before staging")
	}
	staged = true
	if err := applyQueueToRemote(database, job, time.Second); err != nil {
		t.Fatal(err)
	}
	if sent == nil || !reflect.DeepEqual(sent.Needs, []string{spec}) || len(sent.ArtifactNeeds) != 0 {
		t.Fatalf("SSH consumer did not retain marker-based staging: %+v", sent)
	}
}

// An agent built before producer-artifact support advertises artifact-need-v1
// and handles named assets only. Dispatching a producer need to it passes the
// capability gate and then strands the job on the host, waiting for a marker
// nothing will write — the failure wb137 reported, moved one layer down. The
// version field is what separates the two runners; the capability string
// cannot.
func TestR2QueueProducerNeedsRequireVersion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		version    int
		wantRefuse bool
	}{
		{"agent predating the version field", 0, true},
		{"agent advertising named assets only", opsqueue.ArtifactNeedVersionNamedAssets, true},
		{"redeployed agent", opsqueue.ArtifactNeedVersionProducerArtifacts, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, store, publishState := setupR2QueueNeeds(t)
			publishState([]string{opsqueue.CapabilityArtifactNeedV1}, tc.version)
			spec, key := recordNeedProducer(t, database, "")
			r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, candidate string) (bool, error) {
				return candidate == key, nil
			}
			job := recordR2NeedsConsumer(t, database, []string{spec})
			err := AppendJobToQueue(database, job, time.Second)
			adds := r2QueueAdds(t, store)
			if tc.wantRefuse {
				if err == nil || !opsqueue.IsMissingRunnerCapabilityBlock(err.Error()) {
					t.Fatalf("stale runner (version %d): want capability refusal, got %v", tc.version, err)
				}
				if len(adds) != 0 {
					t.Fatalf("stale runner received a producer need it cannot satisfy: %+v", adds)
				}
				return
			}
			if err != nil {
				t.Fatalf("capable runner refused: %v", err)
			}
			if len(adds) != 1 {
				t.Fatalf("capable runner got %d queue adds, want 1", len(adds))
			}
		})
	}
}

// Named assets must keep dispatching to agents that have not been redeployed;
// the version gate applies to the shape they cannot handle, not to all needs.
func TestR2QueueNamedAssetsUnaffectedByVersion(t *testing.T) {
	database, store, publishState := setupR2QueueNeeds(t)
	publishState([]string{opsqueue.CapabilityArtifactNeedV1}, opsqueue.ArtifactNeedVersionNamedAssets)
	if err := db.UpsertNamedAsset(database, db.NamedAsset{Name: "trace", ContentHash: "hash", ContentType: "file", TargetPath: "data/trace"}); err != nil {
		t.Fatal(err)
	}
	job := recordR2NeedsConsumer(t, database, []string{"asset:trace"})
	if err := AppendJobToQueue(database, job, time.Second); err != nil {
		t.Fatalf("named asset refused on a v1 runner: %v", err)
	}
	if adds := r2QueueAdds(t, store); len(adds) != 1 {
		t.Fatalf("got %d queue adds, want 1", len(adds))
	}
}

func TestR2QueueProducerDirectoryExpandsOnlyReadyAuthoritativeAttempt(t *testing.T) {
	for _, tc := range []struct {
		name, readiness, pathPrefix string
	}{
		{name: "ready", readiness: db.PublicationStateReady},
		{name: "pending", readiness: db.PublicationStatePending},
		{name: "failed", readiness: db.PublicationStateFailed},
		{name: "leading slash", readiness: db.PublicationStateReady, pathPrefix: "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, store, _ := setupR2QueueNeeds(t)
			fileSpec, _, runID := recordNeedProducerWithoutPublication(t, database, "")
			var producerID int64
			if _, err := fmt.Sscanf(fileSpec, "outputs/result.bin:%d", &producerID); err != nil {
				t.Fatal(err)
			}
			const directory = "outputs/model"
			spec := fmt.Sprintf("%s%s:%d", tc.pathPrefix, directory, producerID)
			key := r2keys.JobAttemptOutputsPrefix(producerID, runID) + directory
			now := time.Now().Unix()
			if _, err := db.UpsertAttemptPublicationState(database, &db.AttemptPublicationState{
				AttemptID: runID, JobID: producerID, Sequence: 1, ObservedAt: now,
				ExecutionState:         db.PublicationExecutionComplete,
				RequiredArtifactsState: db.PublicationStatePending, DrainState: db.PublicationStatePending,
				Artifacts: []db.AttemptPublicationArtifact{{Name: "model", Path: directory, State: tc.readiness, PayloadKey: key}},
			}); err != nil {
				t.Fatal(err)
			}
			store.objects[key+"/weights.bin"] = []byte("weights")
			store.objects[key+"/nested/config.json"] = []byte("{}")
			store.objects[key+"-backup/leak"] = []byte("sibling")
			store.objects[r2keys.JobAttemptOutputsPrefix(producerID, runID+1)+directory+"/other"] = []byte("wrong attempt")
			job := recordR2NeedsConsumer(t, database, []string{spec})
			err := AppendJobToQueue(database, job, time.Second)
			adds := r2QueueAdds(t, store)
			if tc.readiness != db.PublicationStateReady {
				if err == nil || len(adds) != 0 {
					t.Fatalf("unready directory dispatched: %+v, %v", adds, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []opsqueue.ArtifactNeed{
				{Spec: spec, Path: directory + "/nested/config.json", R2Key: key + "/nested/config.json"},
				{Spec: spec, Path: directory + "/weights.bin", R2Key: key + "/weights.bin"},
			}
			if len(adds) != 1 || !reflect.DeepEqual(adds[0].ArtifactNeeds, want) {
				t.Fatalf("directory queue needs = %+v, want %+v", adds, want)
			}
		})
	}
}

func TestR2QueueProducerReadyPublicationWithoutBytesRefusesDispatch(t *testing.T) {
	database, store, _ := setupR2QueueNeeds(t)
	spec, key := recordNeedProducer(t, database, "")
	delete(store.objects, key)
	job := recordR2NeedsConsumer(t, database, []string{spec})
	err := AppendJobToQueue(database, job, time.Second)
	if !errors.Is(err, r2resolve.ErrArtifactMissing) || len(r2QueueAdds(t, store)) != 0 {
		t.Fatalf("ready publication without bytes dispatched: %v", err)
	}
}

func TestR2QueueUndeclaredDirectoryWaitsForDrainAndRefreshes(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			database, store, _ := setupR2QueueNeeds(t)
			fileSpec, _, runID := recordNeedProducerWithoutPublication(t, database, "")
			var producerID int64
			if _, err := fmt.Sscanf(fileSpec, "outputs/result.bin:%d", &producerID); err != nil {
				t.Fatal(err)
			}
			const directory = "outputs/model"
			spec := fmt.Sprintf("%s:%d", directory, producerID)
			key := r2keys.JobAttemptOutputsPrefix(producerID, runID) + directory
			if _, err := db.UpsertAttemptPublicationState(database, &db.AttemptPublicationState{
				AttemptID: runID, JobID: producerID, Sequence: 1, ObservedAt: time.Now().Unix(),
				ExecutionState:         db.PublicationExecutionComplete,
				RequiredArtifactsState: db.PublicationStateReady,
				DrainState:             db.PublicationStatePending,
			}); err != nil {
				t.Fatal(err)
			}
			if partial {
				store.objects[key+"/weights.bin"] = []byte("weights")
			}
			job := recordR2NeedsConsumer(t, database, []string{spec})
			err := AppendJobToQueue(database, job, time.Second)
			if err == nil || errors.Is(err, r2resolve.ErrArtifactMissing) || len(r2QueueAdds(t, store)) != 0 {
				t.Fatalf("pending drain admitted partial data or reported confirmed absence: %v", err)
			}

			stubAttemptPublicationReport(t, producerID, runID, []byte(
				`{"sequence":2,"observed_at_unix":1700000000,"facets":{"execution_state":"complete","required_artifacts_state":"ready","drain_state":"ready"}}`,
			))
			store.objects[key+"/weights.bin"] = []byte("weights")
			store.objects[key+"/nested/config.json"] = []byte("{}")
			if err := AppendJobToQueue(database, job, time.Second); err != nil {
				t.Fatalf("ready drain did not release the dependency: %v", err)
			}
			adds := r2QueueAdds(t, store)
			want := []opsqueue.ArtifactNeed{
				{Spec: spec, Path: directory + "/nested/config.json", R2Key: key + "/nested/config.json"},
				{Spec: spec, Path: directory + "/weights.bin", R2Key: key + "/weights.bin"},
			}
			if len(adds) != 1 || !reflect.DeepEqual(adds[0].ArtifactNeeds, want) {
				t.Fatalf("drained directory queue needs = %+v, want %+v", adds, want)
			}
		})
	}
}
