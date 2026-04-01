package dashboard

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloudproviders"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/llm"
)

func TestLoadInitialSnapshotUsesCachedState(t *testing.T) {
	database := db.SetupTestDB(t)

	if _, err := database.Exec(
		`INSERT INTO jobs (id, tombstoned, command, working_dir, placement_host) VALUES (1, 0, 'echo hello', '/tmp', 'host-alpha')`,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	database.Exec(`UPDATE job_attempts SET status = ?, host = ? WHERE job_id = 1 AND end_time IS NULL`,
		db.StatusRunning, "host-alpha")

	lastUpdated := time.Now().Add(-10 * time.Minute).Unix()
	if err := db.SaveCachedHostInfo(database, &db.CachedHostInfo{
		Name:        "host-alpha",
		LastUpdated: lastUpdated,
		Arch:        "Linux x86_64",
	}); err != nil {
		t.Fatalf("SaveCachedHostInfo: %v", err)
	}

	syncedAt := time.Now().Add(-5 * time.Minute)
	if err := db.RecordHostSync(database, "host-alpha", syncedAt); err != nil {
		t.Fatalf("RecordHostSync: %v", err)
	}

	snapshot := LoadInitialSnapshot(database, DefaultHostCacheDuration)

	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].ID != 1 {
		t.Fatalf("snapshot.Jobs = %+v, want job 1", snapshot.Jobs)
	}
	if got := snapshot.JobDependencies[1]; got != "" {
		t.Fatalf("snapshot.JobDependencies[1] = %q, want empty", got)
	}
	if len(snapshot.Hosts) != 1 {
		t.Fatalf("len(snapshot.Hosts) = %d, want 1", len(snapshot.Hosts))
	}
	host := snapshot.Hosts[0]
	if host.Name != "host-alpha" {
		t.Fatalf("host.Name = %q, want host-alpha", host.Name)
	}
	if host.Status != HostStatusOnline {
		t.Fatalf("host.Status = %v, want %v", host.Status, HostStatusOnline)
	}
	if got := snapshot.HostSyncTimes["host-alpha"]; got.Sub(syncedAt).Abs() > time.Second {
		t.Fatalf("snapshot.HostSyncTimes[host-alpha] = %v, want within 1s of %v", got, syncedAt)
	}
}

func TestNewModelWithOptionsUsesInitialSnapshot(t *testing.T) {
	database := db.SetupTestDB(t)

	now := time.Now()
	snapshot := InitialSnapshot{
		Jobs: []*db.Job{
			{ID: 7, Host: "host-alpha", Status: db.StatusQueued},
		},
		JobDependencies: map[int64]string{7: "3+"},
		Hosts: []*Host{
			{Name: "host-alpha", Status: HostStatusOnline},
		},
		HostSyncTimes: map[string]time.Time{"host-alpha": now},
	}

	model := NewModelWithOptions(database, ModelOptions{
		InitialSnapshot: &snapshot,
		CloudDiscoveryFn: func(*config.Config) cloudproviders.Discovery {
			panic("cloud discovery should not run during model construction")
		},
		LLMInitFn: func(*sql.DB, *config.Config) *llm.DescriptionGenerator {
			panic("LLM init should not run during model construction")
		},
	})
	defer model.syncWorker.Stop()

	if len(model.allJobs) != 1 || model.allJobs[0].ID != 7 {
		t.Fatalf("model.allJobs = %+v, want job 7", model.allJobs)
	}
	if got := model.jobDependencies[7]; got != "3+" {
		t.Fatalf("model.jobDependencies[7] = %q, want 3+", got)
	}
	if len(model.hosts) != 1 || model.hosts[0].Name != "host-alpha" {
		t.Fatalf("model.hosts = %+v, want host-alpha", model.hosts)
	}
	if got := model.hostSyncTimes["host-alpha"]; got.Sub(now).Abs() > time.Second {
		t.Fatalf("model.hostSyncTimes[host-alpha] = %v, want within 1s of %v", got, now)
	}
}

func TestNewModelWithOptionsDoesNotRunDeferredInitSynchronously(t *testing.T) {
	database := db.SetupTestDB(t)

	cloudCalled := false
	llmCalled := false

	model := NewModelWithOptions(database, ModelOptions{
		CloudDiscoveryFn: func(*config.Config) cloudproviders.Discovery {
			cloudCalled = true
			return cloudproviders.Discovery{}
		},
		LLMInitFn: func(*sql.DB, *config.Config) *llm.DescriptionGenerator {
			llmCalled = true
			return nil
		},
	})
	defer model.syncWorker.Stop()

	if cloudCalled {
		t.Fatal("cloud discovery ran during model construction")
	}
	if llmCalled {
		t.Fatal("LLM init ran during model construction")
	}
}
