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
	"github.com/osteele/weft/internal/r2resolve"
)

func setupR2QueueNeeds(t *testing.T) (*sql.DB, *fakeInventoryQueueStore, func([]string, ...int)) {
	t.Helper()
	database := db.SetupTestDB(t)
	oldLoad, oldStore, oldSSH := loadQueueConfig, newInventoryQueueStore, appendQueueCommandSSH
	oldExists, oldRuns := r2resolve.ObjectExistsFunc, r2resolve.ListRunIDsFunc
	t.Cleanup(func() {
		loadQueueConfig, newInventoryQueueStore, appendQueueCommandSSH = oldLoad, oldStore, oldSSH
		r2resolve.ObjectExistsFunc, r2resolve.ListRunIDsFunc = oldExists, oldRuns
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
	r2resolve.ObjectExistsFunc = func(context.Context, r2resolve.Store, string) (bool, error) {
		t.Fatal("unexpected producer R2 lookup")
		return false, nil
	}
	r2resolve.ListRunIDsFunc = func(context.Context, r2resolve.Lister, int64) ([]int64, error) {
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

func recordNeedProducer(t *testing.T, database *sql.DB, host string) (string, string) {
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
	return fmt.Sprintf("outputs/result.bin:%d", id), fmt.Sprintf("jobs/%d/runs/%d/outputs/outputs/result.bin", id, *producer.LatestRunID)
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
				r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, candidate string) (bool, error) {
					return candidate == key, nil
				}
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
				var key string
				spec, key = recordNeedProducer(t, database, "")
				r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, candidate string) (bool, error) {
					return candidate == key, nil
				}
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

func TestR2QueueProducerDispatchRetriesMissingArtifact(t *testing.T) {
	for _, route := range []string{"host-sync", "reconcile"} {
		t.Run(route, func(t *testing.T) {
			database, store, _ := setupR2QueueNeeds(t)
			spec, key := recordNeedProducer(t, database, "")
			job := recordR2NeedsConsumer(t, database, []string{spec})
			uploaded := false
			r2resolve.ObjectExistsFunc = func(_ context.Context, _ r2resolve.Store, candidate string) (bool, error) {
				return uploaded && candidate == key, nil
			}
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
			if err == nil || !strings.Contains(err.Error(), "not in R2 yet") ||
				!strings.Contains(err.Error(), "outputs/result.bin") || !strings.Contains(err.Error(), "wj1") {
				t.Fatalf("want producer-specific retryable missing-artifact error, got %v", err)
			}
			if route == "reconcile" && !errors.Is(err, r2resolve.ErrArtifactMissing) {
				t.Fatalf("missing-artifact cause lost: %v", err)
			}
			if adds := r2QueueAdds(t, store); len(adds) != 0 {
				t.Fatalf("missing artifact published a queue entry: %+v", adds)
			}
			pending, err := db.GetJobByID(database, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if pending.Status != db.StatusQueued || pending.LastSyncedStatus != "" ||
				pending.FailureReason != "" || pending.ErrorMessage != "" || pending.ExitCode != nil || pending.EndTime != nil {
				t.Fatalf("missing artifact became terminal or dispatched: %+v", pending)
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
			uploaded = true
			if err := dispatch(); err != nil {
				t.Fatalf("retry after upload: %v", err)
			}
			adds := r2QueueAdds(t, store)
			if len(adds) != 1 || len(adds[0].ArtifactNeeds) != 1 || adds[0].ArtifactNeeds[0].R2Key != key {
				t.Fatalf("retry did not publish resolved producer key: %+v", adds)
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
