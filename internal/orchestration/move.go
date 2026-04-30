package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/cloudproviders"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
)

type Option struct {
	IsNew       bool
	InstanceID  int64
	Offer       *cloud.Offer
	Strategy    bidding.SelectionStrategy
	WaitTime    time.Duration
	CostPerHour float64
	GPUName     string
}

type Result struct {
	TargetDesc string
	InstanceID int64
}

type BulkCallbacks struct {
	OnStatus          func(string)
	OnWarning         func(string)
	OnEvent           func(campaign.LaunchEvent)
	OnCampaignCreated func(campaignID int64, expectedWorkers int)
}

type BulkResult struct {
	InstanceIDs []int64
}

func BuildOptions(
	cloudClients []cloud.Client,
	job *db.Job,
	capacities []campaign.InstanceCapacity,
	queuedCounts map[int64]int,
	sourceInstanceID int64,
	minReliability float64,
) ([]Option, error) {
	var options []Option

	var filtered []campaign.InstanceCapacity
	for _, cap := range capacities {
		if cap.Instance.ID == sourceInstanceID {
			continue
		}
		filtered = append(filtered, cap)
	}
	for _, cap := range campaign.RankForJob(job, filtered) {
		inst := cap.Instance
		gpuName := inst.ResolvedGPUName
		if gpuName == "" {
			gpuName = inst.GPUClass
		}
		options = append(options, Option{
			IsNew:       false,
			InstanceID:  inst.ID,
			GPUName:     gpuName,
			WaitTime:    time.Duration(queuedCounts[inst.ID]) * 30 * time.Minute,
			CostPerHour: float64(inst.CostPerHourCents) / 100.0,
		})
	}

	if len(cloudClients) > 0 {
		provider, _ := db.RequestedProvider(job.Tags)
		group := campaign.InstanceGroup{
			GPUClass: job.GPUClass,
			Provider: provider,
			Jobs:     []*db.Job{job},
		}
		if job.GPUMemGB != nil {
			group.GPUMemGB = *job.GPUMemGB
		}
		rawOffers := campaign.FetchGroupRawOffers(cloudClients, []campaign.InstanceGroup{group}, minReliability)
		if len(rawOffers) > 0 && rawOffers[0].Err != nil {
			return nil, rawOffers[0].Err
		}

		strategies := []bidding.SelectionStrategy{
			bidding.StrategyCheap,
			bidding.StrategyFast,
			bidding.StrategyFastest,
		}
		seen := make(map[string]bool)
		for _, strategy := range strategies {
			ranked := campaign.RankGroupOffers(rawOffers, nil, 1.0, nil, strategy, 0)
			if len(ranked) == 0 || ranked[0].Offer == nil {
				continue
			}
			offer := ranked[0].Offer
			key := offer.Key()
			if seen[key] {
				continue
			}
			seen[key] = true
			options = append(options, Option{
				IsNew:       true,
				Offer:       offer,
				Strategy:    strategy,
				GPUName:     offer.GPUName,
				WaitTime:    8 * time.Minute,
				CostPerHour: offer.CostPerHour,
			})
		}
	}
	return options, nil
}

func ExecuteOption(
	ctx context.Context,
	database *sql.DB,
	r2Client *r2.Client,
	cfg *config.Config,
	cloudClients []cloud.Client,
	jobID int64,
	opt Option,
) (string, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return "", fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
	}
	if job == nil {
		return "", fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}

	// Open a MoveIntent so the autopilot leaves this job alone for the
	// duration of the move, and so a failure can be cleanly rolled back to
	// the source. See specs/job-move.allium.
	intent, err := openMoveIntent(database, job, opt)
	if err != nil {
		return "", err
	}

	var sourceLaunchID int64
	if job.LaunchID != nil {
		sourceLaunchID = *job.LaunchID
	}

	desc, err := executeMoveOption(ctx, database, r2Client, cfg, cloudClients, job, opt, intent)
	if err != nil {
		// Best-effort: re-attach to source if it's still alive. Otherwise
		// the job is left unplaced and the autopilot may pick it up on its
		// next pass (the only state where it should).
		restored := tryRestoreJobToSource(database, job.ID, sourceLaunchID)
		resolution := "move failed"
		if restored {
			resolution = "move failed; restored to source"
		}
		if rerr := db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateCanceled, resolution); rerr != nil {
			slog.Warn("resolve move intent canceled", "component", "move", "intent_id", intent.ID, "error", rerr)
		}
		return "", err
	}
	if err := db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateConfirmed, desc); err != nil {
		slog.Warn("resolve move intent confirmed", "component", "move", "intent_id", intent.ID, "error", err)
	}
	return desc, nil
}

// openMoveIntent records the move attempt before any source-mutating action
// runs. Rejects a second move on a job that already has one in flight.
func openMoveIntent(database *sql.DB, job *db.Job, opt Option) (*db.MoveIntent, error) {
	params := db.CreateMoveIntentParams{
		JobID:          job.ID,
		SourceLaunchID: job.LaunchID,
		TargetGPUName:  opt.GPUName,
	}
	if opt.IsNew {
		params.TargetKind = db.MoveTargetNew
		if opt.Offer != nil {
			params.TargetOfferProvider = string(opt.Offer.Provider)
			params.TargetOfferID = opt.Offer.ProviderID
		}
	} else {
		params.TargetKind = db.MoveTargetExisting
		params.TargetLaunchID = &opt.InstanceID
	}
	intent, err := db.CreateMoveIntent(database, params)
	if err != nil {
		return nil, fmt.Errorf("open move intent: %w", err)
	}
	return intent, nil
}

func executeMoveOption(
	ctx context.Context,
	database *sql.DB,
	r2Client *r2.Client,
	cfg *config.Config,
	cloudClients []cloud.Client,
	job *db.Job,
	opt Option,
	intent *db.MoveIntent,
) (string, error) {
	if !opt.IsNew {
		// Move-to-existing supersedes the source claim atomically inside
		// SubmitJobsToInstanceForMove (TransferJobLaunchID), so the source
		// is never visibly unplaced. On failure the rollback inside the
		// submit returns the job to unplaced; the outer cancel-intent path
		// then restores to source if it is still alive.
		if err := campaign.SubmitJobsToInstanceForMove(ctx, database, r2Client, opt.InstanceID, []*db.Job{job}); err != nil {
			return "", fmt.Errorf("submit to instance %s: %w", ids.FormatInstanceID(opt.InstanceID), err)
		}
		return fmt.Sprintf("instance %s", ids.FormatInstanceID(opt.InstanceID)), nil
	}

	// Move-to-new uses TransferClaim to atomically supersede the source
	// claim *inside* LaunchCampaign — the prior unplace-then-launch
	// sequence is gone. If the launch fails (no offer, provider error)
	// before the instance is created, source is untouched. If the instance
	// is created and the agent later stalls, the existing
	// reconcile.bootstrap_timeout path terminates the instance and resets
	// the job back to queued — at which point the autopilot can re-place.
	if opt.Offer == nil {
		return "", fmt.Errorf("new-instance option missing offer")
	}

	offer := *opt.Offer
	group := campaign.InstanceGroup{GPUClass: job.GPUClass, Jobs: []*db.Job{job}}
	if job.GPUMemGB != nil {
		group.GPUMemGB = *job.GPUMemGB
	}

	gracePeriod := 5 * time.Minute
	if cfg != nil {
		if gp := cfg.DefaultGracePeriod(); gp != "" && gp != "0" {
			if d, parseErr := time.ParseDuration(gp); parseErr == nil {
				gracePeriod = d
			}
		}
	}
	launchOpts := campaign.LaunchOpts{
		GracePeriodSeconds: int(gracePeriod.Seconds()),
		Strategy:           opt.Strategy,
		TransferClaim:      true,
	}

	var r2Cfg cloud.R2Config
	if cfg != nil {
		r2Cfg = cfg.Vastai.R2.ToCloudR2Config()
	}
	client := cloudClientForProvider(cloudClients, offer.Provider)
	if client == nil {
		return "", fmt.Errorf("no client for provider %s", offer.Provider)
	}
	createOpts, err := createOptsForProvider(cfg, offer.Provider)
	if err != nil {
		return "", fmt.Errorf("create opts: %w", err)
	}
	result, err := campaign.LaunchCampaign(
		[]cloud.Client{client},
		database,
		[]campaign.InstanceGroup{group},
		[]cloud.Offer{offer},
		nil,
		nil,
		launchOpts,
		r2Cfg,
		func(cloud.Provider) (cloud.CreateOpts, error) { return createOpts, nil },
		func(ev campaign.LaunchEvent) {
			if ev.InstanceID > 0 && intent != nil {
				_ = db.UpdateMoveIntentTargetLaunch(database, intent.ID, ev.InstanceID)
			}
		},
		nil,
		func(_ campaign.InstanceGroup, instanceID int64) {
			if intent != nil {
				_ = db.UpdateMoveIntentTargetLaunch(database, intent.ID, instanceID)
			}
		},
	)
	if err != nil {
		return "", fmt.Errorf("launch: %w", err)
	}
	desc := fmt.Sprintf("new %s instance", opt.GPUName)
	if len(result.InstanceIDs) > 0 {
		desc = fmt.Sprintf("new %s instance %s", opt.GPUName, ids.FormatInstanceID(result.InstanceIDs[0]))
	}
	return desc, nil
}

// tryRestoreJobToSource re-attaches the job to its source launch if that
// instance is still alive. Returns true on a successful restore. Returns
// false (and leaves the job unplaced) if the source is unknown, dead, or the
// re-attach fails.
func tryRestoreJobToSource(database *sql.DB, jobID, sourceLaunchID int64) bool {
	if sourceLaunchID <= 0 {
		return false
	}
	src, err := db.GetLaunch(database, sourceLaunchID)
	if err != nil || src == nil || !db.IsLiveLaunchStatus(src.Status) {
		return false
	}
	if err := db.SetJobLaunchID(database, jobID, sourceLaunchID); err != nil {
		slog.Warn("restore job to source failed",
			"component", "move", "job_id", jobID, "source_launch_id", sourceLaunchID, "error", err)
		return false
	}
	oplog.LogJob("move.restored_to_source", jobID, "",
		oplog.WithDetailf("source_launch_id=%d", sourceLaunchID))
	return true
}

func MoveQueuedJobToNewInstance(database *sql.DB, jobID int64) (Result, error) {
	moveStarted := time.Now()
	logPhase := func(phase string, started time.Time, detail string, err error) {
		msg := "cmd=jobs move target=new phase=" + phase
		if detail != "" {
			msg += " " + detail
		}
		opts := []oplog.Option{oplog.WithDetail(msg), oplog.WithDuration(time.Since(started))}
		if err != nil {
			opts = append(opts, oplog.WithError(err))
		}
		oplog.LogJob(oplog.OpCLICommand, jobID, "", opts...)
	}
	oplog.LogJob(oplog.OpCLICommand, jobID, "", oplog.WithDetail("cmd=jobs move target=new phase=start"))

	jobLoadStarted := time.Now()
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		logPhase("load_job", jobLoadStarted, "", err)
		return Result{}, fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
	}
	if job == nil {
		logPhase("load_job", jobLoadStarted, "", fmt.Errorf("job not found"))
		return Result{}, fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	logPhase("load_job", jobLoadStarted, fmt.Sprintf("status=%s host=%s", job.EffectiveStatus(), job.Host), nil)
	if job.EffectiveStatus() != db.StatusQueued {
		return Result{}, fmt.Errorf("can only move queued jobs (job %s has status: %s)", ids.FormatJobID(jobID), job.EffectiveStatus())
	}

	configStarted := time.Now()
	cfg, err := config.Load()
	if err != nil {
		logPhase("load_config", configStarted, "", err)
		return Result{}, fmt.Errorf("load config: %w", err)
	}
	logPhase("load_config", configStarted, "", nil)

	clientsStarted := time.Now()
	cloudClients, err := BuildCloudClients(cfg)
	if err != nil {
		logPhase("build_cloud_clients", clientsStarted, "", err)
		return Result{}, fmt.Errorf("build cloud clients: %w", err)
	}
	logPhase("build_cloud_clients", clientsStarted, fmt.Sprintf("providers=%d", len(cloudClients)), nil)

	r2Started := time.Now()
	r2Client, err := BuildR2Client(cfg)
	if err != nil {
		logPhase("build_r2_client", r2Started, "", err)
		return Result{}, fmt.Errorf("build R2 client: %w", err)
	}
	logPhase("build_r2_client", r2Started, fmt.Sprintf("enabled=%t", r2Client != nil), nil)

	launchesStarted := time.Now()
	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		logPhase("list_running_launches", launchesStarted, "", err)
		return Result{}, fmt.Errorf("list running launches: %w", err)
	}
	logPhase("list_running_launches", launchesStarted, fmt.Sprintf("count=%d", len(launches)), nil)

	capacityStarted := time.Now()
	capacities := make([]campaign.InstanceCapacity, 0, len(launches))
	queuedCounts := make(map[int64]int, len(launches))
	for _, ci := range launches {
		liveJobs, jobsErr := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		if jobsErr != nil {
			continue
		}
		if cap, ok := campaign.NewInstanceCapacity(ci, countRunningJobsForMove(liveJobs)); ok {
			capacities = append(capacities, cap)
		}
		for _, j := range liveJobs {
			if j != nil && j.EffectiveStatus() == db.StatusQueued {
				queuedCounts[ci.ID]++
			}
		}
	}
	logPhase("build_capacities", capacityStarted, fmt.Sprintf("capacities=%d queued_counts=%d", len(capacities), len(queuedCounts)), nil)

	var sourceInstanceID int64
	if job.LaunchID != nil {
		sourceInstanceID = *job.LaunchID
	}

	optionsStarted := time.Now()
	options, err := BuildOptions(cloudClients, job, capacities, queuedCounts, sourceInstanceID, cfg.CampaignReliability())
	if err != nil {
		logPhase("lookup_options", optionsStarted, "", err)
		return Result{}, err
	}
	logPhase("lookup_options", optionsStarted, fmt.Sprintf("options=%d", len(options)), nil)

	var selected *Option
	for i := range options {
		if options[i].IsNew {
			selected = &options[i]
			break
		}
	}
	if selected == nil {
		return Result{}, fmt.Errorf("no compatible new-instance destination found")
	}

	execStarted := time.Now()
	targetDesc, err := ExecuteOption(context.Background(), database, r2Client, cfg, cloudClients, job.ID, *selected)
	if err != nil {
		logPhase("execute_move", execStarted, "", err)
		return Result{}, err
	}
	logPhase("execute_move", execStarted, targetDesc, nil)

	result := Result{TargetDesc: strings.TrimSpace(targetDesc)}
	if result.TargetDesc != "" {
		for _, token := range strings.Fields(result.TargetDesc) {
			clean := strings.Trim(token, ",.;:()[]")
			id, parseErr := ids.ParseInstanceID(clean)
			if parseErr == nil && id > 0 {
				result.InstanceID = id
				break
			}
		}
	}
	logPhase("complete", moveStarted, fmt.Sprintf("target=%s instance=%d", result.TargetDesc, result.InstanceID), nil)
	return result, nil
}

func MoveQueuedJobsToNewInstances(database *sql.DB, jobs []*db.Job, separateEach bool, cb BulkCallbacks) (BulkResult, error) {
	moveStarted := time.Now()
	logPhase := func(phase string, started time.Time, detail string, err error) {
		msg := "cmd=jobs move target=new phase=" + phase
		if detail != "" {
			msg += " " + detail
		}
		opts := []oplog.Option{oplog.WithDetail(msg), oplog.WithDuration(time.Since(started))}
		if err != nil {
			opts = append(opts, oplog.WithError(err))
		}
		oplog.Log(oplog.OpCLICommand, opts...)
	}
	oplog.Log(oplog.OpCLICommand, oplog.WithDetailf("cmd=jobs move target=new phase=start jobs=%d each=%t", len(jobs), separateEach))

	cfgStarted := time.Now()
	cfg, err := config.Load()
	if err != nil {
		logPhase("load_config", cfgStarted, "", err)
		return BulkResult{}, fmt.Errorf("load config: %w", err)
	}
	logPhase("load_config", cfgStarted, "", nil)

	clientsStarted := time.Now()
	clients, err := BuildCloudClients(cfg)
	if err != nil {
		logPhase("build_cloud_clients", clientsStarted, "", err)
		return BulkResult{}, fmt.Errorf("no cloud providers available: %w", err)
	}
	if len(clients) == 0 {
		noProvidersErr := fmt.Errorf("no cloud providers available")
		logPhase("build_cloud_clients", clientsStarted, "providers=0", noProvidersErr)
		return BulkResult{}, noProvidersErr
	}
	logPhase("build_cloud_clients", clientsStarted, fmt.Sprintf("providers=%d", len(clients)), nil)

	r2Started := time.Now()
	r2Client, r2Err := BuildR2Client(cfg)
	if r2Err != nil {
		logPhase("build_r2_client", r2Started, "", r2Err)
		return BulkResult{}, fmt.Errorf("R2 client: %w", r2Err)
	}
	logPhase("build_r2_client", r2Started, fmt.Sprintf("enabled=%t", r2Client != nil), nil)

	unplaceStarted := time.Now()
	unplaced := 0
	for _, job := range jobs {
		if err := unplaceIfNeeded(database, job); err != nil {
			if cb.OnWarning != nil {
				cb.OnWarning(fmt.Sprintf("Warning: unplace job %s failed: %v", ids.FormatJobID(job.ID), err))
			}
			continue
		}
		if err := db.SetPendingStatus(database, job.ID, db.StatusPendingPlacement); err != nil {
			if cb.OnWarning != nil {
				cb.OnWarning(fmt.Sprintf("Warning: set pending placement for job %s failed: %v", ids.FormatJobID(job.ID), err))
			}
			continue
		}
		unplaced++
	}
	logPhase("unplace_jobs", unplaceStarted, fmt.Sprintf("ok=%d total=%d", unplaced, len(jobs)), nil)

	refreshStarted := time.Now()
	launchable, warnings := refreshLaunchableJobs(database, jobs)
	for _, warning := range warnings {
		if cb.OnWarning != nil {
			cb.OnWarning(warning)
		}
	}
	logPhase("refresh_launchable", refreshStarted, fmt.Sprintf("launchable=%d warnings=%d", len(launchable), len(warnings)), nil)

	launchableIDs := make([]int64, 0, len(launchable))
	for _, job := range launchable {
		if job != nil {
			launchableIDs = append(launchableIDs, job.ID)
		}
	}
	markedJobIDs, err := markJobsPendingPlacement(database, launchableIDs)
	if err != nil {
		return BulkResult{}, err
	}
	defer func() {
		if restoreErr := restorePendingPlacementToQueued(database, markedJobIDs); restoreErr != nil {
			slog.Warn("failed to normalize pending_placement jobs", "component", "move", "error", restoreErr)
		}
	}()

	groupStarted := time.Now()
	var groups []campaign.InstanceGroup
	groupCandidates := jobsForGrouping(launchable)
	if separateEach {
		for _, job := range groupCandidates {
			groups = append(groups, campaign.PrepareGroups([]*db.Job{job}, database, "", r2Client)...)
		}
	} else {
		groups = campaign.PrepareGroups(groupCandidates, database, "", r2Client)
	}
	if len(groups) == 0 {
		groupErr := fmt.Errorf("no launchable groups from provided jobs")
		logPhase("prepare_groups", groupStarted, "groups=0", groupErr)
		return BulkResult{}, groupErr
	}
	logPhase("prepare_groups", groupStarted, fmt.Sprintf("groups=%d jobs=%d", len(groups), countGroupJobs(groups)), nil)

	offersStarted := time.Now()
	groupOffers := campaign.FetchGroupOffers(clients, groups, nil, 1.0, nil, bidding.StrategyCheap, cfg.CampaignReliability(), 0.4)
	logPhase("fetch_offers", offersStarted, fmt.Sprintf("group_offers=%d", len(groupOffers)), nil)

	selectStarted := time.Now()
	var launchGroups []campaign.InstanceGroup
	var launchOffers []cloud.Offer
	for _, gOffer := range groupOffers {
		if gOffer.Offer == nil || gOffer.Err != nil {
			detail := "no offers"
			if gOffer.Err != nil {
				detail = gOffer.Err.Error()
			}
			if cb.OnWarning != nil {
				cb.OnWarning(fmt.Sprintf("Warning: %s for %s (%d jobs) — skipping", detail, gOffer.Group.GPUSpec(), len(gOffer.Group.Jobs)))
			}
			continue
		}
		launchGroups = append(launchGroups, gOffer.Group)
		launchOffers = append(launchOffers, *gOffer.Offer)
	}
	if len(launchGroups) == 0 {
		noOffersErr := fmt.Errorf("no cloud offers found for any GPU group")
		logPhase("select_offers", selectStarted, "launch_groups=0", noOffersErr)
		return BulkResult{}, noOffersErr
	}
	logPhase("select_offers", selectStarted, fmt.Sprintf("launch_groups=%d", len(launchGroups)), nil)

	if cb.OnStatus != nil {
		cb.OnStatus(fmt.Sprintf("Launching %d instance(s) for %d job(s)...", len(launchGroups), countGroupJobs(launchGroups)))
	}
	groupLabels := buildMoveGroupProgressLabels(launchGroups)
	opts := campaign.LaunchOpts{
		GracePeriodSeconds: 15 * 60,
		Strategy:           bidding.StrategyCheap,
		MinSurvival:        0.4,
	}
	launchStarted := time.Now()
	result, err := campaign.LaunchCampaign(
		clients,
		database,
		launchGroups,
		launchOffers,
		nil,
		nil,
		opts,
		cfg.Vastai.R2.ToCloudR2Config(),
		func(provider cloud.Provider) (cloud.CreateOpts, error) {
			return createOptsForProvider(cfg, provider)
		},
		func(event campaign.LaunchEvent) {
			if cb.OnEvent != nil {
				cb.OnEvent(event)
			}
			if cb.OnStatus != nil {
				if line := formatMoveLaunchEventLine(event, groupLabels); line != "" {
					cb.OnStatus(line)
				}
			}
		},
		func(id int64) {
			if cb.OnCampaignCreated != nil {
				cb.OnCampaignCreated(id, len(launchGroups))
			}
			if cb.OnStatus != nil {
				cb.OnStatus(fmt.Sprintf("Campaign %d: launching %d instance(s)...", id, len(launchGroups)))
			}
		},
		nil,
	)
	if err != nil {
		logPhase("launch_campaign", launchStarted, "", err)
		return BulkResult{}, err
	}
	logPhase("launch_campaign", launchStarted, fmt.Sprintf("instance_ids=%d errors=%d", len(result.InstanceIDs), len(result.Errors)), nil)

	for _, e := range result.Errors {
		if cb.OnWarning != nil {
			cb.OnWarning(fmt.Sprintf("Warning: %v", e))
		}
	}
	if len(result.InstanceIDs) == 0 && len(result.Errors) > 0 {
		return BulkResult{}, result.Errors[0]
	}
	if cb.OnStatus != nil {
		for _, id := range result.InstanceIDs {
			cb.OnStatus(fmt.Sprintf("Launched instance %s", ids.FormatInstanceID(id)))
		}
	}
	logPhase("complete", moveStarted, fmt.Sprintf("instances=%d", len(result.InstanceIDs)), nil)
	return BulkResult{InstanceIDs: result.InstanceIDs}, nil
}

func formatMoveLaunchEventLine(event campaign.LaunchEvent, groupLabels map[string]string) string {
	switch event.Kind {
	case campaign.LaunchEventCampaignStatus:
		phase := strings.TrimSpace(event.Phase)
		if phase == "" {
			return ""
		}
		return "  " + phase
	case campaign.LaunchEventGroupAssets:
		group := event.Group
		prefix := strings.TrimSpace(group.GPUSpec())
		if label := moveGroupProgressLabel(group, groupLabels); label != "" {
			prefix = strings.TrimSpace(label + " " + prefix)
		}
		if prefix == "" {
			return ""
		}
		return fmt.Sprintf("  %s: staging (%d/%d assets ready)", prefix, event.AssetsReady, event.AssetsTotal)
	case campaign.LaunchEventGroupRetry:
		group := event.Group
		prefix := strings.TrimSpace(group.GPUSpec())
		if label := moveGroupProgressLabel(group, groupLabels); label != "" {
			prefix = strings.TrimSpace(label + " " + prefix)
		}
		if prefix == "" {
			return ""
		}
		return fmt.Sprintf("  %s: retrying with replacement offer (attempt %d/%d)", prefix, event.RetryAttempt, event.RetryMax)
	case campaign.LaunchEventGroupPhase:
		group := event.Group
		phase := strings.TrimSpace(event.Phase)
		prefix := strings.TrimSpace(group.GPUSpec())
		if label := moveGroupProgressLabel(group, groupLabels); label != "" {
			prefix = strings.TrimSpace(label + " " + prefix)
		}
		if prefix == "" || phase == "" {
			return ""
		}
		return fmt.Sprintf("  %s: %s", prefix, phase)
	default:
		return ""
	}
}

func BuildCloudClients(cfg *config.Config) ([]cloud.Client, error) {
	discovery := cloudproviders.Discover(cfg)
	return discovery.Clients, discovery.UnavailableError()
}

func BuildR2Client(cfg *config.Config) (*r2.Client, error) {
	if cfg == nil {
		return nil, nil
	}
	r2Cfg := cfg.Vastai.R2
	if r2Cfg.Bucket == "" || r2Cfg.AccessKeyID == "" {
		return nil, nil
	}
	return r2.New(r2.Config{
		AccountID:       r2Cfg.AccountID,
		AccessKeyID:     r2Cfg.AccessKeyID,
		SecretAccessKey: r2Cfg.SecretAccessKey,
		Bucket:          r2Cfg.Bucket,
	})
}

func cloudClientForProvider(clients []cloud.Client, provider cloud.Provider) cloud.Client {
	for _, c := range clients {
		if c.Provider() == provider {
			return c
		}
	}
	if len(clients) == 1 {
		return clients[0]
	}
	return nil
}

func createOptsForProvider(cfg *config.Config, provider cloud.Provider) (cloud.CreateOpts, error) {
	if cfg == nil {
		return cloud.CreateOpts{}, fmt.Errorf("cloud config is required")
	}
	return cfg.CloudCreateOpts(provider)
}

func countRunningJobsForMove(jobs []*db.Job) int {
	count := 0
	for _, j := range jobs {
		if j != nil && j.EffectiveStatus() == db.StatusRunning {
			count++
		}
	}
	return count
}

func unplaceIfNeeded(database *sql.DB, job *db.Job) error {
	if job == nil {
		return nil
	}
	if job.TargetKind() == db.JobTargetUnplaced {
		return nil
	}
	_, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast))
	return err
}

func refreshLaunchableJobs(database *sql.DB, jobs []*db.Job) ([]*db.Job, []string) {
	launchable := make([]*db.Job, 0, len(jobs))
	var warnings []string
	for _, job := range jobs {
		if job == nil {
			continue
		}
		latest, err := db.GetJobByID(database, job.ID)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Warning: reload job %s failed: %v", ids.FormatJobID(job.ID), err))
			continue
		}
		if latest == nil {
			warnings = append(warnings, fmt.Sprintf("Warning: job %s no longer exists, skipping", ids.FormatJobID(job.ID)))
			continue
		}
		status := latest.EffectiveStatus()
		if status != db.StatusQueued && status != db.StatusPendingPlacement {
			warnings = append(warnings, fmt.Sprintf("Warning: job %s has status %s after unplace, skipping", ids.FormatJobID(job.ID), status))
			continue
		}
		if latest.HasAssignedHost() {
			warnings = append(warnings, fmt.Sprintf("Warning: job %s is still placed after unplace, skipping", ids.FormatJobID(job.ID)))
			continue
		}
		launchable = append(launchable, latest)
	}
	return launchable, warnings
}

func RefreshLaunchableJobs(database *sql.DB, jobs []*db.Job) ([]*db.Job, []string) {
	return refreshLaunchableJobs(database, jobs)
}

func jobsForGrouping(jobs []*db.Job) []*db.Job {
	grouping := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if job.EffectiveStatus() == db.StatusPendingPlacement {
			clone := *job
			clone.PendingStatus = nil
			grouping = append(grouping, &clone)
			continue
		}
		grouping = append(grouping, job)
	}
	return grouping
}

func countGroupJobs(groups []campaign.InstanceGroup) int {
	n := 0
	for _, g := range groups {
		n += len(g.Jobs)
	}
	return n
}

func buildMoveGroupProgressLabels(groups []campaign.InstanceGroup) map[string]string {
	if len(groups) == 0 {
		return nil
	}
	labels := make(map[string]string, len(groups))
	for i, group := range groups {
		anchor := int64(0)
		for _, job := range group.Jobs {
			if job == nil {
				continue
			}
			if anchor == 0 || job.ID < anchor {
				anchor = job.ID
			}
		}
		label := fmt.Sprintf("[%d/%d]", i+1, len(groups))
		if anchor > 0 {
			label = fmt.Sprintf("[%d/%d wj%d]", i+1, len(groups), anchor)
		}
		labels[moveGroupSignature(group)] = label
	}
	return labels
}

func moveGroupProgressLabel(group campaign.InstanceGroup, labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	return labels[moveGroupSignature(group)]
}

func moveGroupSignature(group campaign.InstanceGroup) string {
	ids := make([]int64, 0, len(group.Jobs))
	for _, job := range group.Jobs {
		if job == nil || job.ID <= 0 {
			continue
		}
		ids = append(ids, job.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var b strings.Builder
	b.WriteString(strings.TrimSpace(group.GPUSpec()))
	b.WriteString("|")
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(fmt.Sprintf("%d", id))
	}
	return b.String()
}
