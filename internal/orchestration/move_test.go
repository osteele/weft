package orchestration

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

func TestBuildMoveGroupProgressLabels_UsesOrdinalAndAnchorJobID(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "NVIDIA",
			Jobs: []*db.Job{
				{ID: 1069},
			},
		},
		{
			GPUClass: "NVIDIA",
			Jobs: []*db.Job{
				{ID: 1063},
				{ID: 1090},
			},
		},
	}

	labels := buildMoveGroupProgressLabels(groups)
	if got := moveGroupProgressLabel(groups[0], labels); got != "[1/2 wj1069]" {
		t.Fatalf("label for group 1 = %q, want %q", got, "[1/2 wj1069]")
	}
	if got := moveGroupProgressLabel(groups[1], labels); got != "[2/2 wj1063]" {
		t.Fatalf("label for group 2 = %q, want %q", got, "[2/2 wj1063]")
	}
}

func TestBuildMoveGroupProgressLabels_FallsBackToOrdinalWhenNoJobIDs(t *testing.T) {
	groups := []campaign.InstanceGroup{
		{
			GPUClass: "NVIDIA",
			Jobs:     []*db.Job{{ID: 0}, nil},
		},
	}

	labels := buildMoveGroupProgressLabels(groups)
	if got := moveGroupProgressLabel(groups[0], labels); got != "[1/1]" {
		t.Fatalf("label = %q, want %q", got, "[1/1]")
	}
}

func TestRefreshLaunchableJobs_AcceptsPendingPlacement(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "python train.py", "pending placement move")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	staleJob, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if err := db.MoveQueuedJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("MoveQueuedJobToUnplaced: %v", err)
	}
	if err := db.SetPendingStatus(database, jobID, db.StatusPendingPlacement); err != nil {
		t.Fatalf("SetPendingStatus: %v", err)
	}

	launchable, warnings := refreshLaunchableJobs(database, []*db.Job{staleJob})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if len(launchable) != 1 {
		t.Fatalf("launchable = %d, want 1", len(launchable))
	}
	if got := launchable[0].EffectiveStatus(); got != db.StatusPendingPlacement {
		t.Fatalf("effective status = %q, want %q", got, db.StatusPendingPlacement)
	}
}

func TestBuildOptions_NewOffersRespectJobMaxComputeCap(t *testing.T) {
	job := &db.Job{
		ID:            1838,
		Status:        db.StatusQueued,
		GPUClass:      "NVIDIA",
		MaxComputeCap: "9.0",
	}
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(c cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{
				{
					ProviderID:  "blackwell",
					Provider:    cloud.ProviderVastai,
					GPUName:     "RTX PRO 6000 WS",
					CostPerHour: 0.10,
				},
				{
					ProviderID:  "ampere",
					Provider:    cloud.ProviderVastai,
					GPUName:     "RTX A6000",
					CostPerHour: 0.20,
				},
			}, nil
		},
	}

	options, err := BuildOptions([]cloud.Client{client}, job, nil, nil, 0, 0)
	if err != nil {
		t.Fatalf("BuildOptions: %v", err)
	}
	foundA6000 := false
	for _, opt := range options {
		if !opt.IsNew {
			continue
		}
		if opt.GPUName == "RTX PRO 6000 WS" {
			t.Fatalf("incompatible Blackwell offer was returned: %+v", opt)
		}
		if opt.GPUName == "RTX A6000" {
			foundA6000 = true
		}
	}
	if !foundA6000 {
		t.Fatalf("expected compatible RTX A6000 option, got %+v", options)
	}
}

func TestUnplaceIfNeeded_AlreadyUnplaced(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "unplaced job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.TargetKind() != db.JobTargetUnplaced {
		t.Fatalf("setup: TargetKind = %q, want %q", job.TargetKind(), db.JobTargetUnplaced)
	}

	if err := unplaceIfNeeded(database, job); err != nil {
		t.Fatalf("unplaceIfNeeded on already-unplaced job: %v", err)
	}
}

func TestUnplaceIfNeeded_RentalSourceLeftAttached(t *testing.T) {
	// Regression: bulk move-to-new must not detach rental-source jobs
	// before LaunchCampaign supersedes the claim. See unplaceIfNeeded.
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "rental job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if !job.IsRentalJob() {
		t.Fatalf("setup: IsRentalJob() = false, want true (LaunchID = %v)", job.LaunchID)
	}

	if err := unplaceIfNeeded(database, job); err != nil {
		t.Fatalf("unplaceIfNeeded on rental job: %v", err)
	}

	after, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after: %v", err)
	}
	if after.LaunchID == nil || *after.LaunchID != src {
		t.Fatalf("rental job lost its source claim: launch_id = %v, want %d", after.LaunchID, src)
	}
}

func TestOpenBulkMoveIntentsRestoresRetryableNoStartFailureToSource(t *testing.T) {
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	dst, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "rental job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}

	intentIDs, err := openMoveIntentsForNewInstanceGroups(database, []campaign.InstanceGroup{{GPUClass: "A100", Jobs: []*db.Job{job}}}, []cloud.Offer{{Provider: cloud.ProviderVastai, ProviderID: "offer-1", GPUName: "A100"}})
	if err != nil {
		t.Fatalf("openMoveIntentsForNewInstanceGroups: %v", err)
	}
	if intentIDs[jobID] == 0 {
		t.Fatal("expected move intent id for job")
	}
	if err := db.UpdateMoveIntentTargetLaunch(database, intentIDs[jobID], dst); err != nil {
		t.Fatalf("UpdateMoveIntentTargetLaunch: %v", err)
	}
	if err := db.TransferJobLaunchID(database, jobID, dst); err != nil {
		t.Fatalf("TransferJobLaunchID target: %v", err)
	}

	n, err := db.ResetLaunchJobs(database, dst, db.AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("reset count = %d, want 1", n)
	}
	after, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after: %v", err)
	}
	if after.LaunchID == nil || *after.LaunchID != src {
		t.Fatalf("after launch_id = %v, want source %d while retry budget remains", after.LaunchID, src)
	}
	intent, err := db.GetMoveIntent(database, intentIDs[jobID])
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if intent.State != db.MoveIntentStateOpen {
		t.Fatalf("intent state = %q, want open", intent.State)
	}
}

func TestJobsForGrouping_ClonesPendingPlacementAsQueued(t *testing.T) {
	pending := db.StatusPendingPlacement
	original := &db.Job{ID: 1, Status: db.StatusQueued, PendingStatus: &pending}

	grouping := jobsForGrouping([]*db.Job{original})
	if len(grouping) != 1 {
		t.Fatalf("grouping len = %d, want 1", len(grouping))
	}
	if grouping[0] == original {
		t.Fatal("expected pending job to be cloned for grouping")
	}
	if got := grouping[0].EffectiveStatus(); got != db.StatusQueued {
		t.Fatalf("grouping effective status = %q, want %q", got, db.StatusQueued)
	}
	if got := original.EffectiveStatus(); got != db.StatusPendingPlacement {
		t.Fatalf("original effective status = %q, want %q", got, db.StatusPendingPlacement)
	}
}

func TestTryRestoreJobToSource_RestoresWhenSourceAlive(t *testing.T) {
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python a.py", "j", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, src); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	// Simulate the move's intermediate "unplace" step: the job loses its
	// launch association. The intent retains the source.
	if err := db.ResetJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("ResetJobToUnplaced: %v", err)
	}

	if !tryRestoreJobToSource(database, jobID, src) {
		t.Fatal("tryRestoreJobToSource returned false; expected restore to succeed")
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != src {
		t.Fatalf("after restore: launch_id = %v, want %d", job.LaunchID, src)
	}
}

func TestTryRestoreJobToSource_DoesNotRestoreWhenSourceTerminal(t *testing.T) {
	database := db.SetupTestDB(t)

	src, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusFailed})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python a.py", "j", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if tryRestoreJobToSource(database, jobID, src) {
		t.Fatal("tryRestoreJobToSource returned true; expected no restore for failed source")
	}
}

func TestTryRestoreJobToSource_NoSourceLaunchIDIsNoOp(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python a.py", "j", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if tryRestoreJobToSource(database, jobID, 0) {
		t.Fatal("tryRestoreJobToSource returned true; expected no-op when source unknown")
	}
}

func TestLaunchNewForJob_SelectsRankedOfferAndPassesStrategy(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "launch now", "nvidia")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, "nvidia"); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}

	var gotOpt Option
	restore := stubExecuteMoveOptionForMove(t, func(
		_ context.Context,
		_ *sql.DB,
		_ *r2.Client,
		_ *config.Config,
		_ []cloud.Client,
		_ *db.Job,
		opt Option,
		_ *db.MoveIntent,
	) (string, error) {
		gotOpt = opt
		return "new RTX 3090 instance wi77", nil
	})
	defer restore()

	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
			if constraints.GPUClass != "nvidia" {
				t.Fatalf("GPUClass constraint = %q, want nvidia", constraints.GPUClass)
			}
			return []cloud.Offer{
				{ProviderID: "expensive", Provider: cloud.ProviderVastai, GPUName: "A100", GPUMemGB: 40, CostPerHour: 2.00},
				{ProviderID: "cheap", Provider: cloud.ProviderVastai, GPUName: "RTX 3090", GPUMemGB: 24, CostPerHour: 0.20},
			}, nil
		},
	}

	desc, err := LaunchNewForJob(context.Background(), database, nil, nil, []cloud.Client{client}, jobID, bidding.StrategyFast)
	if err != nil {
		t.Fatalf("LaunchNewForJob: %v", err)
	}
	if desc == "" {
		t.Fatal("desc is empty")
	}
	if gotOpt.Strategy != bidding.StrategyFast {
		t.Fatalf("strategy = %q, want %q", gotOpt.Strategy, bidding.StrategyFast)
	}
	if gotOpt.Offer == nil || gotOpt.Offer.ProviderID != "cheap" {
		t.Fatalf("selected offer = %#v, want provider_id cheap", gotOpt.Offer)
	}
}

func TestLaunchNewForJob_NoOffers(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "launch now", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	client := &cloud.MockClient{
		ProviderVal:      cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) { return nil, nil },
	}
	_, err = LaunchNewForJob(context.Background(), database, nil, nil, []cloud.Client{client}, jobID, bidding.StrategyFast)
	if err == nil || !strings.Contains(err.Error(), "no compatible new-instance offer") {
		t.Fatalf("LaunchNewForJob err = %v, want no-offer error", err)
	}
}

func TestLaunchNewForJob_JobNotFound(t *testing.T) {
	database := db.SetupTestDB(t)
	_, err := LaunchNewForJob(context.Background(), database, nil, nil, nil, 999, bidding.StrategyFast)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("LaunchNewForJob err = %v, want not found", err)
	}
}

func TestLaunchNewForJob_JobNotQueued(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "launch now", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetRequestedStatus(database, jobID, db.StatusKilled); err != nil {
		t.Fatalf("SetRequestedStatus: %v", err)
	}

	_, err = LaunchNewForJob(context.Background(), database, nil, nil, nil, jobID, bidding.StrategyFast)
	if err == nil || !strings.Contains(err.Error(), "can only launch queued jobs") {
		t.Fatalf("LaunchNewForJob err = %v, want status error", err)
	}
}

func TestLaunchNewForJob_ExecuteFailureResolvesIntentCanceled(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "launch now", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	execErr := errors.New("provider rejected")
	restore := stubExecuteMoveOptionForMove(t, func(
		context.Context,
		*sql.DB,
		*r2.Client,
		*config.Config,
		[]cloud.Client,
		*db.Job,
		Option,
		*db.MoveIntent,
	) (string, error) {
		return "", execErr
	})
	defer restore()

	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		SearchOffersFunc: func(cloud.OfferConstraints) ([]cloud.Offer, error) {
			return []cloud.Offer{{ProviderID: "offer", Provider: cloud.ProviderVastai, GPUName: "A100", GPUMemGB: 40, CostPerHour: 1}}, nil
		},
	}

	_, err = LaunchNewForJob(context.Background(), database, nil, nil, []cloud.Client{client}, jobID, bidding.StrategyFast)
	if !errors.Is(err, execErr) {
		t.Fatalf("LaunchNewForJob err = %v, want %v", err, execErr)
	}
	var state string
	if err := database.QueryRow(`SELECT state FROM move_intents WHERE job_id = ?`, jobID).Scan(&state); err != nil {
		t.Fatalf("query move intent state: %v", err)
	}
	if db.MoveIntentState(state) != db.MoveIntentStateCanceled {
		t.Fatalf("move intent state = %q, want %q", state, db.MoveIntentStateCanceled)
	}
}

func stubExecuteMoveOptionForMove(t *testing.T, fn func(context.Context, *sql.DB, *r2.Client, *config.Config, []cloud.Client, *db.Job, Option, *db.MoveIntent) (string, error)) func() {
	t.Helper()
	previous := executeMoveOptionForMove
	executeMoveOptionForMove = fn
	return func() {
		executeMoveOptionForMove = previous
	}
}
