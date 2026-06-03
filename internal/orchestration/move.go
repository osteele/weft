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
	// InstanceIDs lists every successfully launched instance.
	InstanceIDs []int64
	// PlacedJobIDs lists jobs that landed on one of InstanceIDs. Empty when
	// the move never reached the launch stage.
	PlacedJobIDs []int64
	// UnplacedJobs lists jobs that were intended to land on a new instance
	// but didn't, with the best-available reason. A job is "unplaced" when
	// either offer search produced nothing for its group or the per-group
	// launch ultimately failed after retries.
	UnplacedJobs []UnplacedJob
	// RequestedEach is true when the caller asked for one instance per job
	// (e.g. --to distinct / --each). Surfacing the user's intent lets the
	// receipt formatter say "Placed M of N distinct instances" precisely.
	RequestedEach bool
	// RequestedCount is the number of distinct instances the caller asked
	// for. For RequestedEach it equals the launchable job count; for the
	// merged planner branch it equals the planner's group output count.
	RequestedCount int
}

// UnplacedJob pairs a job ID with the user-visible reason it didn't reach
// an instance, drawn from the offer-search or launch-failure path that
// dropped it.
type UnplacedJob struct {
	JobID  int64
	Reason string
}

func BuildOptions(
	cloudClients []cloud.Client,
	job *db.Job,
	capacities []campaign.InstanceCapacity,
	queuedCounts map[int64]int,
	sourceInstanceID int64,
	minReliability float64,
) ([]Option, error) {
	return BuildOptionsWithSurvival(cloudClients, job, capacities, queuedCounts, sourceInstanceID, minReliability, nil, 0, nil)
}

func BuildOptionsWithSurvival(
	cloudClients []cloud.Client,
	job *db.Job,
	capacities []campaign.InstanceCapacity,
	queuedCounts map[int64]int,
	sourceInstanceID int64,
	minReliability float64,
	survivalModel *bidding.SurvivalModel,
	minSurvival float64,
	excludedMachineIDs map[string]struct{},
) ([]Option, error) {
	options := BuildExistingOptions(job, capacities, queuedCounts, sourceInstanceID)
	newOptions, err := BuildNewOptionsWithSurvival(cloudClients, job, minReliability, survivalModel, minSurvival, excludedMachineIDs)
	if err != nil {
		return nil, err
	}
	options = append(options, newOptions...)
	return options, nil
}

func BuildExistingOptions(
	job *db.Job,
	capacities []campaign.InstanceCapacity,
	queuedCounts map[int64]int,
	sourceInstanceID int64,
) []Option {
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
			WaitTime:    existingInstanceMoveWait(cap, queuedCounts[inst.ID]),
			CostPerHour: float64(inst.CostPerHourCents) / 100.0,
		})
	}
	return options
}

func BuildNewOptionsWithSurvival(
	cloudClients []cloud.Client,
	job *db.Job,
	minReliability float64,
	survivalModel *bidding.SurvivalModel,
	minSurvival float64,
	excludedMachineIDs map[string]struct{},
) ([]Option, error) {
	if len(cloudClients) == 0 {
		return nil, nil
	}

	provider, _ := db.RequestedProvider(job.Tags)
	group := campaign.InstanceGroup{
		GPUClass:      job.GPUClass,
		Provider:      provider,
		MaxComputeCap: campaign.GroupMaxComputeCap(nil, []*db.Job{job}),
		MinComputeCap: campaign.GroupMinComputeCap([]*db.Job{job}),
		Jobs:          []*db.Job{job},
	}
	if job.GPUMemGB != nil {
		group.GPUMemGB = *job.GPUMemGB
	}
	rawOffers := campaign.FetchGroupRawOffers(cloudClients, []campaign.InstanceGroup{group}, minReliability)
	if len(rawOffers) > 0 && rawOffers[0].Err != nil {
		return nil, rawOffers[0].Err
	}
	for i := range rawOffers {
		rawOffers[i].Offers = filterOffersByMachine(rawOffers[i].Offers, excludedMachineIDs)
	}

	strategies := []bidding.SelectionStrategy{
		bidding.StrategyCheap,
		bidding.StrategyFast,
		bidding.StrategyFastest,
	}
	var options []Option
	seen := make(map[string]bool)
	for _, strategy := range strategies {
		ranked := campaign.RankGroupOffers(rawOffers, survivalModel, 1.0, nil, strategy, minSurvival)
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
	return options, nil
}

func existingInstanceMoveWait(cap campaign.InstanceCapacity, queuedJobs int) time.Duration {
	jobsAhead := cap.RunningJobCount + queuedJobs
	if jobsAhead <= 0 {
		return 0
	}
	return time.Duration(jobsAhead) * 30 * time.Minute
}

func filterOffersByMachine(offers []cloud.Offer, excludedMachineIDs map[string]struct{}) []cloud.Offer {
	if len(offers) == 0 || len(excludedMachineIDs) == 0 {
		return offers
	}
	filtered := offers[:0]
	for _, offer := range offers {
		if offer.MachineID != "" {
			if _, excluded := excludedMachineIDs[offer.MachineID]; excluded {
				continue
			}
		}
		filtered = append(filtered, offer)
	}
	return filtered
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

	return executeWithIntent(ctx, database, r2Client, cfg, cloudClients, job, opt)
}

// LaunchNewForJob picks an offer using the given strategy and launches a new
// instance for the job, transferring the source claim atomically. This is the
// atomic new-instance path used by the TUI's N keybinding: no picker.
func LaunchNewForJob(
	ctx context.Context,
	database *sql.DB,
	r2Client *r2.Client,
	cfg *config.Config,
	cloudClients []cloud.Client,
	jobID int64,
	strategy bidding.SelectionStrategy,
) (string, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return "", fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
	}
	if job == nil {
		return "", fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	if !isMoveAdmissibleStatus(job.EffectiveStatus(), true) {
		return "", fmt.Errorf("cannot launch new instance for job %s in status %s", ids.FormatJobID(jobID), job.EffectiveStatus())
	}

	provider, _ := db.RequestedProvider(job.Tags)
	group := campaign.InstanceGroup{
		GPUClass:      job.GPUClass,
		Provider:      provider,
		MaxComputeCap: campaign.GroupMaxComputeCap(database, []*db.Job{job}),
		MinComputeCap: campaign.GroupMinComputeCap([]*db.Job{job}),
		Jobs:          []*db.Job{job},
	}
	if job.GPUMemGB != nil {
		group.GPUMemGB = *job.GPUMemGB
	}

	rawOffers := campaign.FetchGroupRawOffers(cloudClients, []campaign.InstanceGroup{group}, cfg.CampaignReliability())
	if len(rawOffers) > 0 && rawOffers[0].Err != nil {
		return "", rawOffers[0].Err
	}
	ranked := campaign.RankGroupOffers(rawOffers, nil, 1.0, nil, strategy, 0)
	if len(ranked) == 0 || ranked[0].Offer == nil {
		return "", fmt.Errorf("no compatible new-instance offer found")
	}

	offer := ranked[0].Offer
	return executeWithIntent(ctx, database, r2Client, cfg, cloudClients, job, Option{
		IsNew:       true,
		Offer:       offer,
		Strategy:    strategy,
		GPUName:     offer.GPUName,
		CostPerHour: offer.CostPerHour,
	})
}

var executeMoveOptionForMove = executeMoveOption

func executeWithIntent(
	ctx context.Context,
	database *sql.DB,
	r2Client *r2.Client,
	cfg *config.Config,
	cloudClients []cloud.Client,
	job *db.Job,
	opt Option,
) (string, error) {
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
	// Snapshot any on-prem running source BEFORE the move runs. After
	// claim transfer the DB no longer carries the source attempt's
	// (host, start_time); we need this snapshot to SSH-kill the source
	// process when the move commits.
	sources := SnapshotForcedSources([]*db.Job{job})

	desc, err := executeMoveOptionForMove(ctx, database, r2Client, cfg, cloudClients, job, opt, intent)
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
	if !opt.IsNew {
		if err := db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateConfirmed, desc); err != nil {
			slog.Warn("resolve move intent confirmed", "component", "move", "intent_id", intent.ID, "error", err)
		}
	}
	TerminateForcedSources(database, sources, func(msg string) {
		slog.Warn("terminate forced source", "component", "move", "job_id", job.ID, "msg", msg)
	})
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
		params.AttemptCount = 1
		params.MaxAttempts = defaultMoveToNewMaxAttempts()
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
	group := campaign.InstanceGroup{
		GPUClass:      job.GPUClass,
		MaxComputeCap: campaign.GroupMaxComputeCap(database, []*db.Job{job}),
		MinComputeCap: campaign.GroupMinComputeCap([]*db.Job{job}),
		Jobs:          []*db.Job{job},
	}
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
				if err := db.AttachMoveIntentTargetLaunch(database, intent, ev.InstanceID); err != nil {
					slog.Warn("attach move target launch", "component", "move", "job_id", intent.JobID, "launch_id", ev.InstanceID, "error", err)
				}
			}
		},
		nil,
		func(_ campaign.InstanceGroup, instanceID int64) {
			if intent != nil {
				if err := db.AttachMoveIntentTargetLaunch(database, intent, instanceID); err != nil {
					slog.Warn("attach move target launch", "component", "move", "job_id", intent.JobID, "launch_id", instanceID, "error", err)
				}
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

func MoveQueuedJobToNewInstance(database *sql.DB, jobID int64, force bool) (Result, error) {
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
	if !isMoveAdmissibleStatus(job.EffectiveStatus(), force) {
		return Result{}, fmt.Errorf("can only move queued jobs (job %s has status: %s); pass --force to move running jobs", ids.FormatJobID(jobID), job.EffectiveStatus())
	}
	sources := SnapshotForcedSources([]*db.Job{job})

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
	options, err := BuildOptionsWithSurvival(cloudClients, job, capacities, queuedCounts, sourceInstanceID, cfg.CampaignReliability(), buildSurvivalModel(database), 0.4, nil)
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
	TerminateForcedSources(database, sources, func(msg string) {
		slog.Warn("terminate forced source", "component", "move", "job_id", jobID, "msg", msg)
	})
	return result, nil
}

func MoveQueuedJobsToNewInstances(database *sql.DB, jobs []*db.Job, separateEach bool, force bool, cb BulkCallbacks) (BulkResult, error) {
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

	refreshStarted := time.Now()
	launchable, warnings := refreshMoveToNewJobs(database, jobs, force)
	for _, warning := range warnings {
		if cb.OnWarning != nil {
			cb.OnWarning(warning)
		}
	}
	logPhase("refresh_launchable", refreshStarted, fmt.Sprintf("launchable=%d warnings=%d", len(launchable), len(warnings)), nil)

	sources := SnapshotForcedSources(launchable)

	groupStarted := time.Now()
	var groups []campaign.InstanceGroup
	groupCandidates := jobsForGrouping(launchable)
	if separateEach {
		for _, job := range groupCandidates {
			groups = append(groups, campaign.PrepareGroupsWithConfig([]*db.Job{job}, database, cfg, "", r2Client)...)
		}
	} else {
		groups = campaign.PrepareGroupsWithConfig(groupCandidates, database, cfg, "", r2Client)
	}
	if len(groups) == 0 {
		groupErr := fmt.Errorf("no launchable groups from provided jobs")
		logPhase("prepare_groups", groupStarted, "groups=0", groupErr)
		return BulkResult{}, groupErr
	}
	logPhase("prepare_groups", groupStarted, fmt.Sprintf("groups=%d jobs=%d", len(groups), countGroupJobs(groups)), nil)

	offersStarted := time.Now()
	var launchGroups []campaign.InstanceGroup
	var launchOffers []cloud.Offer
	// preDropped collects unplaced jobs that fell out of the run before the
	// launch stage (offer search returned nothing for their group, or the
	// planner could not satisfy them). These are merged with launch-time
	// failures into BulkResult.UnplacedJobs at the end of the function so
	// the receipt distinguishes "requested but no offers" from "requested
	// and launch failed".
	var preDropped []UnplacedJob
	planLabel := "none"
	requestedCount := 0
	if separateEach {
		// User explicitly asked for one instance per job. Skip the planner so
		// MergeCompatibleGroups can't undo that intent; resolve offers directly.
		groupOffers := campaign.FetchGroupOffers(clients, groups, nil, 1.0, nil, bidding.StrategyCheap, cfg.CampaignReliability(), 0.4)
		logPhase("fetch_offers", offersStarted, fmt.Sprintf("group_offers=%d mode=each", len(groupOffers)), nil)
		requestedCount = len(groupOffers)

		for _, gOffer := range groupOffers {
			if gOffer.Offer == nil || gOffer.Err != nil {
				detail := "no offers"
				if gOffer.Err != nil {
					detail = gOffer.Err.Error()
				}
				if cb.OnWarning != nil {
					cb.OnWarning(fmt.Sprintf("Warning: %s for %s (%d jobs) — skipping", detail, gOffer.Group.GPUSpec(), len(gOffer.Group.Jobs)))
				}
				preDropped = append(preDropped, unplacedJobsFromGroup(gOffer.Group, detail)...)
				continue
			}
			launchGroups = append(launchGroups, gOffer.Group)
			launchOffers = append(launchOffers, *gOffer.Offer)
		}
		planLabel = "each"
	} else {
		// Route through the planner so the split/merged/parallel candidate
		// evaluation runs. This is what makes overlap-aware grouping per
		// specs/campaign-lifecycle.allium § AssetOverlapLaunchGrouping apply
		// to `weft move --to new`. Reuse is force-disabled by the
		// PrepareNewInstanceLaunchPlan wrapper.
		profile := bidding.StrategyCheap.Profile()
		predCfg := buildPredictorConfig(cfg)
		overheadModel := buildOverheadModel(database)
		survivalModel := buildSurvivalModel(database)
		prep, planErr := campaign.PrepareNewInstanceLaunchPlan(
			database, clients, nil, groups, nil,
			profile, 0.4, cfg.CampaignReliability(),
			&predCfg, overheadModel, survivalModel, nil,
		)
		if planErr != nil {
			logPhase("plan", offersStarted, "", planErr)
			return BulkResult{}, planErr
		}
		if prep.StrategyPlan.NewCandidate != nil {
			planLabel = prep.StrategyPlan.NewCandidate.Label
		}
		logPhase("plan", offersStarted, fmt.Sprintf("candidate=%s launch_groups=%d", planLabel, len(prep.LaunchGroups)), nil)

		launchGroups = prep.LaunchGroups
		launchOffers = prep.Offers
		requestedCount = len(launchGroups)

		// Surface offer failures to the user so they know which jobs were
		// dropped from the launch (and why). Map the winning candidate's
		// per-group results back to the original split groups so the warning
		// names the user-visible job grouping rather than a synthetic
		// merged/parallel group identity. When the planner picked no
		// candidate at all, emit one warning per split group: no candidate
		// means no offers anywhere.
		if prep.StrategyPlan.NewCandidate == nil {
			for _, sg := range groups {
				detail := "no offers"
				if cb.OnWarning != nil {
					cb.OnWarning(fmt.Sprintf("Warning: %s for %s (%d jobs) — skipping", detail, sg.GPUSpec(), len(sg.Jobs)))
				}
				preDropped = append(preDropped, unplacedJobsFromGroup(sg, detail)...)
			}
			requestedCount = len(groups)
		} else {
			projected := campaign.MapOffersToSplitGroups(groups, *prep.StrategyPlan.NewCandidate)
			for i, sg := range groups {
				if i >= len(projected) {
					break
				}
				po := projected[i]
				if po.Offer != nil && po.Err == nil {
					continue
				}
				detail := "no offers"
				if po.Err != nil {
					detail = po.Err.Error()
				}
				if cb.OnWarning != nil {
					cb.OnWarning(fmt.Sprintf("Warning: %s for %s (%d jobs) — skipping", detail, sg.GPUSpec(), len(sg.Jobs)))
				}
				preDropped = append(preDropped, unplacedJobsFromGroup(sg, detail)...)
			}
		}
	}
	if len(launchGroups) == 0 {
		noOffersErr := fmt.Errorf("no cloud offers found for any GPU group")
		logPhase("select", offersStarted, fmt.Sprintf("launch_groups=0 candidate=%s", planLabel), noOffersErr)
		return BulkResult{}, noOffersErr
	}

	if cb.OnStatus != nil {
		cb.OnStatus(fmt.Sprintf("Launching %d instance(s) for %d job(s)...", len(launchGroups), countGroupJobs(launchGroups)))
	}
	groupLabels := buildMoveGroupProgressLabels(launchGroups)
	opts := campaign.LaunchOpts{
		GracePeriodSeconds: 15 * 60,
		Strategy:           bidding.StrategyCheap,
		MinSurvival:        0.4,
		// Rental-source claim is transferred atomically; see unplaceIfNeeded.
		TransferClaim: true,
	}
	launchStarted := time.Now()
	// Track per-group launch outcomes so the receipt can attribute success
	// and failure to specific jobs. The campaign layer reports outcomes per
	// group via LaunchEventGroupDone / LaunchEventGroupFailed; matching by
	// LaunchGroupSignature is stable across the goroutines that fire them.
	groupOutcomes := newGroupOutcomeTracker(launchGroups)
	execution, err := executeNewInstanceLaunchWithMoveIntents(newInstanceLaunchExecutionOptions{
		Database:   database,
		Clients:    clients,
		Groups:     launchGroups,
		Offers:     launchOffers,
		LaunchOpts: opts,
		R2Config:   cfg.Vastai.R2.ToCloudR2Config(),
		CreateOptions: func(provider cloud.Provider) (cloud.CreateOpts, error) {
			return createOptsForProvider(cfg, provider)
		},
		OnEvent: func(event campaign.LaunchEvent) {
			groupOutcomes.observe(event)
			if cb.OnEvent != nil {
				cb.OnEvent(event)
			}
			if cb.OnStatus != nil {
				if line := formatMoveLaunchEventLine(event, groupLabels); line != "" {
					cb.OnStatus(line)
				}
			}
		},
		OnCampaignCreated: func(id int64) {
			if cb.OnCampaignCreated != nil {
				cb.OnCampaignCreated(id, len(launchGroups))
			}
			if cb.OnStatus != nil {
				cb.OnStatus(fmt.Sprintf("Launching %d instance(s) in batch %d...", len(launchGroups), id))
			}
		},
	})
	if err != nil {
		logPhase("launch_campaign", launchStarted, "", err)
		return BulkResult{}, err
	}
	result := execution.Result
	logPhase("launch_campaign", launchStarted, fmt.Sprintf("instance_ids=%d errors=%d", len(result.InstanceIDs), len(result.Errors)), nil)

	for _, e := range result.Errors {
		if cb.OnWarning != nil {
			cb.OnWarning(fmt.Sprintf("Warning: %v", e))
		}
	}
	if len(result.InstanceIDs) == 0 && len(result.Errors) > 0 {
		execution.Cancel("bulk move launch failed")
		return BulkResult{}, result.Errors[0]
	}
	if cb.OnStatus != nil {
		for _, id := range result.InstanceIDs {
			cb.OnStatus(fmt.Sprintf("Launched instance %s", ids.FormatInstanceID(id)))
		}
	}
	logPhase("complete", moveStarted, fmt.Sprintf("instances=%d", len(result.InstanceIDs)), nil)
	// Leave the move_intents open. The CLI/TUI only waited for provider
	// acceptance here; the autopilot owns the longer "target reached
	// agent_ready or retry replacement launch" lifecycle.
	placed, unplaced := groupOutcomes.partition()
	unplaced = append(unplaced, preDropped...)
	// SSH-kill on-prem source processes for every job whose TransferClaim
	// has already closed the source attempt in the DB. Jobs whose group
	// failed offer search or launch are filtered out (their source attempt
	// is still open and we must leave the source process running).
	placedSet := make(map[int64]struct{}, len(placed))
	for _, id := range placed {
		placedSet[id] = struct{}{}
	}
	survivingSources := make([]SourceSnapshot, 0, len(sources))
	for _, snap := range sources {
		if _, ok := placedSet[snap.JobID]; ok {
			survivingSources = append(survivingSources, snap)
		}
	}
	TerminateForcedSources(database, survivingSources, func(msg string) {
		if cb.OnWarning != nil {
			cb.OnWarning(msg)
		}
	})
	return BulkResult{
		InstanceIDs:    result.InstanceIDs,
		PlacedJobIDs:   placed,
		UnplacedJobs:   unplaced,
		RequestedEach:  separateEach,
		RequestedCount: requestedCount,
	}, nil
}

// unplacedJobsFromGroup builds an UnplacedJob entry for every job in the
// group, sharing a single reason string. Used at offer-search drop sites
// where one group's worth of jobs all fail for the same reason.
func unplacedJobsFromGroup(group campaign.InstanceGroup, reason string) []UnplacedJob {
	out := make([]UnplacedJob, 0, len(group.Jobs))
	for _, job := range group.Jobs {
		if job == nil || job.ID <= 0 {
			continue
		}
		out = append(out, UnplacedJob{JobID: job.ID, Reason: reason})
	}
	return out
}

// groupOutcomeTracker matches LaunchEventGroupDone / LaunchEventGroupFailed
// events back to the originally submitted launch groups by signature, so
// the orchestration layer can report per-job outcomes without changing the
// campaign layer's flat (InstanceIDs, Errors) result shape.
type groupOutcomeTracker struct {
	pending map[string]campaign.InstanceGroup // signature -> group, awaiting outcome
	placed  []int64
	failed  []UnplacedJob
}

func newGroupOutcomeTracker(groups []campaign.InstanceGroup) *groupOutcomeTracker {
	t := &groupOutcomeTracker{pending: make(map[string]campaign.InstanceGroup, len(groups))}
	for _, g := range groups {
		t.pending[campaign.LaunchGroupSignature(g)] = g
	}
	return t
}

func (t *groupOutcomeTracker) observe(event campaign.LaunchEvent) {
	switch event.Kind {
	case campaign.LaunchEventGroupDone:
		sig := campaign.LaunchGroupSignature(event.Group)
		group, ok := t.pending[sig]
		if !ok {
			return
		}
		delete(t.pending, sig)
		for _, job := range group.Jobs {
			if job == nil || job.ID <= 0 {
				continue
			}
			t.placed = append(t.placed, job.ID)
		}
	case campaign.LaunchEventGroupFailed:
		sig := campaign.LaunchGroupSignature(event.Group)
		group, ok := t.pending[sig]
		if !ok {
			return
		}
		delete(t.pending, sig)
		reason := strings.TrimSpace(event.Reason)
		if reason == "" {
			reason = "launch failed"
		}
		for _, job := range group.Jobs {
			if job == nil || job.ID <= 0 {
				continue
			}
			t.failed = append(t.failed, UnplacedJob{JobID: job.ID, Reason: reason})
		}
	}
}

// partition returns the placed job IDs and the unplaced jobs the tracker
// has observed. Groups whose outcome never fired are reported as unplaced
// with a "launch outcome unknown" reason — this shouldn't happen with the
// current campaign code, but defending against it keeps the receipt honest.
func (t *groupOutcomeTracker) partition() ([]int64, []UnplacedJob) {
	for _, group := range t.pending {
		for _, job := range group.Jobs {
			if job == nil || job.ID <= 0 {
				continue
			}
			t.failed = append(t.failed, UnplacedJob{JobID: job.ID, Reason: "launch outcome not reported"})
		}
	}
	t.pending = nil
	return t.placed, t.failed
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
	case campaign.LaunchEventGroupReplan:
		group := event.Group
		prefix := strings.TrimSpace(group.GPUSpec())
		if label := moveGroupProgressLabel(group, groupLabels); label != "" {
			prefix = strings.TrimSpace(label + " " + prefix)
		}
		if prefix == "" {
			return ""
		}
		return fmt.Sprintf("  %s: replanning with fresh offer (chain %d/%d)", prefix, event.RetryAttempt, event.RetryMax)
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
		if j == nil {
			continue
		}
		status := j.EffectiveStatus()
		if status == db.StatusRunning || status == db.StatusStarting {
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
	// Rental-source jobs stay attached to source until LaunchCampaign with
	// TransferClaim=true atomically supersedes the source claim at
	// instance-creation time. See specs/job-move.allium §
	// SourceLaunchUnchangedWhileIntentOpen.
	if job.IsRentalJob() {
		return nil
	}
	_, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast))
	return err
}

func refreshMoveToNewJobs(database *sql.DB, jobs []*db.Job, force bool) ([]*db.Job, []string) {
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
		if !isMoveAdmissibleStatus(status, force) {
			warnings = append(warnings, fmt.Sprintf("Warning: job %s has status %s, skipping", ids.FormatJobID(job.ID), status))
			continue
		}
		launchable = append(launchable, latest)
	}
	return launchable, warnings
}

func RefreshLaunchableJobs(database *sql.DB, jobs []*db.Job) ([]*db.Job, []string) {
	return refreshMoveToNewJobs(database, jobs, false)
}

// isMoveAdmissibleStatus reports whether a job's effective status is allowed
// into the move-to-new pipeline. Queued/pending_placement always qualify;
// the running set (running/starting/paused) is admitted only under --force,
// in which case the destination's TransferClaim atomically supersedes the
// source attempt and TerminateForcedSources cleans up the source process.
func isMoveAdmissibleStatus(status string, force bool) bool {
	switch status {
	case db.StatusQueued, db.StatusPendingPlacement:
		return true
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return force
	default:
		return false
	}
}

func jobsForGrouping(jobs []*db.Job) []*db.Job {
	grouping := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if job.EffectiveStatus() == db.StatusPendingPlacement || job.HasInventoryHost() {
			clone := *job
			clone.PendingStatus = nil
			clone.Host = ""
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
