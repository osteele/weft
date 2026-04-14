package terminal

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func queueProjectLaunchJob(t *testing.T, database *sql.DB, project string) int64 {
	t.Helper()

	id, err := db.RecordQueuedWithGPU(database, "", "/tmp/"+project, "python train.py", project+" job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(%s): %v", project, err)
	}
	if _, err := database.Exec(`UPDATE jobs SET project = ? WHERE id = ?`, project, id); err != nil {
		t.Fatalf("set project for %s: %v", project, err)
	}
	return id
}

func TestProjectWatchRouterPrepareLaunchFiltersToProject(t *testing.T) {
	database := db.SetupTestDB(t)

	alphaID := queueProjectLaunchJob(t, database, "ALPHA")
	betaID := queueProjectLaunchJob(t, database, "BETA")

	cfg := &config.Config{
		Vastai: config.VastaiConfig{
			R2: config.R2Config{
				Bucket:      "bucket",
				AccessKeyID: "key",
			},
		},
	}
	router := newProjectWatchRouterModel(database, cfg, 24*time.Hour, false, "ALPHA", false)

	msg, ok := router.prepareLaunch()().(launchPlanReadyMsg)
	if !ok {
		t.Fatal("expected launchPlanReadyMsg")
	}
	if msg.err != nil {
		t.Fatalf("prepareLaunch error: %v", msg.err)
	}
	if msg.model == nil {
		t.Fatal("expected launch model")
	}
	if msg.model.projectFilter != "ALPHA" {
		t.Fatalf("projectFilter = %q, want ALPHA", msg.model.projectFilter)
	}
	if len(msg.model.groups) != 1 {
		t.Fatalf("group count = %d, want 1", len(msg.model.groups))
	}
	if len(msg.model.groups[0].Jobs) != 1 {
		t.Fatalf("group jobs = %d, want 1", len(msg.model.groups[0].Jobs))
	}
	if got := msg.model.groups[0].Jobs[0].ID; got != alphaID {
		t.Fatalf("launch job = %d, want %d", got, alphaID)
	}
	if got := msg.model.groups[0].Jobs[0].ID; got == betaID {
		t.Fatalf("launch unexpectedly included beta job %d", betaID)
	}

	home := router.buildHomeWatch("")
	if home.projectSyncing {
		t.Fatal("project watch sync setting was not preserved")
	}
}

func TestLaunchModelRunReconciliationFiltersToProject(t *testing.T) {
	database := db.SetupTestDB(t)

	alphaID := queueProjectLaunchJob(t, database, "ALPHA")
	_ = queueProjectLaunchJob(t, database, "BETA")

	msg, ok := (launchModel{
		database:      database,
		appConfig:     &config.Config{},
		reconciler:    campaign.NewReconciler(),
		projectFilter: "ALPHA",
	}).runReconciliation()().(reconcileDoneMsg)
	if !ok {
		t.Fatal("expected reconcileDoneMsg")
	}
	if len(msg.groups) != 1 {
		t.Fatalf("group count = %d, want 1", len(msg.groups))
	}
	if len(msg.groups[0].Jobs) != 1 {
		t.Fatalf("group jobs = %d, want 1", len(msg.groups[0].Jobs))
	}
	if got := msg.groups[0].Jobs[0].ID; got != alphaID {
		t.Fatalf("reconciled job = %d, want %d", got, alphaID)
	}
}

func TestWatchModelBuildInstanceCapacitiesIncludesInstanceMode(t *testing.T) {
	m := watchModel{
		mode:        watchModeInstances,
		instanceIDs: []int64{7},
		updates: map[int64]campaign.InstanceUpdate{
			7: {
				Launch: &db.Launch{
					ID:       7,
					Status:   db.LaunchStatusRunning,
					Provider: "vastai",
					GPUSpec:  "A100",
				},
				Jobs: []*db.Job{
					{ID: 11, Status: db.StatusRunning},
				},
			},
		},
	}

	caps := m.buildInstanceCapacities()
	if len(caps) != 1 {
		t.Fatalf("capacity count = %d, want 1", len(caps))
	}
	if caps[0].Instance.ID != 7 {
		t.Fatalf("capacity instance id = %d, want 7", caps[0].Instance.ID)
	}
	if caps[0].RunningJobCount != 1 {
		t.Fatalf("running job count = %d, want 1", caps[0].RunningJobCount)
	}
}

func TestLaunchModelUpdate_SwitchToLaunchEscapesInlineWatch(t *testing.T) {
	inlineWatch := watchModel{}

	model, cmd := launchModel{
		inlineWatch:     &inlineWatch,
		inlineWatchUsed: true,
	}.Update(switchToLaunchMsg{})
	got := model.(launchModel)

	if got.inlineWatch != nil {
		t.Fatal("expected inline watch to close before reconciliation")
	}
	if got.inlineWatchUsed {
		t.Fatal("expected inlineWatchUsed to be reset")
	}
	if !got.reconciling {
		t.Fatal("expected reconciliation to start")
	}
	if cmd == nil {
		t.Fatal("expected reconciliation command")
	}
}

func TestWatchRouterSwitchToLaunchPreservesCurrentInstanceIDs(t *testing.T) {
	database := db.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := watchRouterModel{
		active: watchModel{
			mode:        watchModeInstances,
			database:    database,
			instanceIDs: []int64{7, 11},
			ctx:         ctx,
			cancel:      cancel,
		},
		database:    database,
		config:      &config.Config{},
		homeMode:    watchModeInstances,
		instanceIDs: []int64{7},
	}

	next, _ := router.Update(switchToLaunchMsg{})
	got := next.(watchRouterModel)
	if len(got.instanceIDs) != 2 || got.instanceIDs[0] != 7 || got.instanceIDs[1] != 11 {
		t.Fatalf("router instance IDs = %v, want [7 11]", got.instanceIDs)
	}
}

func TestWatchRouterSwitchToListFromWatchUsesRequestedGrouping(t *testing.T) {
	database := db.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	router := watchRouterModel{
		active: watchModel{
			mode:        watchModeSystem,
			database:    database,
			ctx:         ctx,
			cancel:      cancel,
			syncWorker:  nil,
			dbWatcher:   nil,
			instanceIDs: []int64{7},
		},
		database:  database,
		config:    &config.Config{},
		homeMode:  watchModeSystem,
		listTitle: "Jobs",
		listSync:  true,
	}

	next, _ := router.Update(switchToListMsg{groupedByStatus: true})
	got := next.(watchRouterModel)
	list, ok := got.active.(listTUIModel)
	if !ok {
		t.Fatalf("active model = %T, want listTUIModel", got.active)
	}
	if !list.groupedByStatus {
		t.Fatal("expected grouped list")
	}
}

func TestWatchRouterSwitchToSystemWatchFromList(t *testing.T) {
	database := db.SetupTestDB(t)
	list := listTUIModel{
		database: database,
		cancel:   func() {},
	}
	router := watchRouterModel{
		active:    list,
		database:  database,
		config:    &config.Config{},
		listTitle: "Jobs",
		listSync:  true,
	}

	next, _ := router.Update(switchToSystemWatchMsg{})
	got := next.(watchRouterModel)
	watch, ok := got.active.(watchModel)
	if !ok {
		t.Fatalf("active model = %T, want watchModel", got.active)
	}
	if watch.mode != watchModeSystem {
		t.Fatalf("watch mode = %v, want watchModeSystem", watch.mode)
	}
}
