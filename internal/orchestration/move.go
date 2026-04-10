package orchestration

import (
	"context"
	"database/sql"
	"fmt"
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
	OnStatus  func(string)
	OnWarning func(string)
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
		group := campaign.InstanceGroup{
			GPUClass: job.GPUClass,
			Jobs:     []*db.Job{job},
		}
		if job.GPUMemGB != nil {
			group.GPUMemGB = *job.GPUMemGB
		}
		rawOffers := campaign.FetchGroupRawOffers(cloudClients, []campaign.InstanceGroup{group})
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
		return "", fmt.Errorf("get job %d: %w", jobID, err)
	}
	if job == nil {
		return "", fmt.Errorf("job %d not found", jobID)
	}
	if _, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast)); err != nil {
		return "", fmt.Errorf("unplace: %w", err)
	}
	job, err = db.GetJobByID(database, jobID)
	if err != nil {
		return "", fmt.Errorf("reload job %d: %w", jobID, err)
	}

	if !opt.IsNew {
		if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, opt.InstanceID, []*db.Job{job}); err != nil {
			return "", fmt.Errorf("submit to instance %s: %w", ids.FormatInstanceID(opt.InstanceID), err)
		}
		return fmt.Sprintf("instance %s", ids.FormatInstanceID(opt.InstanceID)), nil
	}

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
		func(campaign.InstanceGroup, string) {},
		nil,
		func(campaign.InstanceGroup, int64) {},
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
		return Result{}, fmt.Errorf("get job %d: %w", jobID, err)
	}
	if job == nil {
		logPhase("load_job", jobLoadStarted, "", fmt.Errorf("job not found"))
		return Result{}, fmt.Errorf("job %d not found", jobID)
	}
	logPhase("load_job", jobLoadStarted, fmt.Sprintf("status=%s host=%s", job.EffectiveStatus(), job.Host), nil)
	if job.EffectiveStatus() != db.StatusQueued {
		return Result{}, fmt.Errorf("can only move queued jobs (job %d has status: %s)", jobID, job.EffectiveStatus())
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
	options, err := BuildOptions(cloudClients, job, capacities, queuedCounts, sourceInstanceID)
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
				cb.OnWarning(fmt.Sprintf("Warning: unplace job %d failed: %v", job.ID, err))
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

	groupStarted := time.Now()
	var groups []campaign.InstanceGroup
	if separateEach {
		for _, job := range launchable {
			groups = append(groups, campaign.PrepareGroups([]*db.Job{job}, database, "", r2Client)...)
		}
	} else {
		groups = campaign.PrepareGroups(launchable, database, "", r2Client)
	}
	if len(groups) == 0 {
		groupErr := fmt.Errorf("no launchable groups from provided jobs")
		logPhase("prepare_groups", groupStarted, "groups=0", groupErr)
		return BulkResult{}, groupErr
	}
	logPhase("prepare_groups", groupStarted, fmt.Sprintf("groups=%d jobs=%d", len(groups), countGroupJobs(groups)), nil)

	offersStarted := time.Now()
	groupOffers := campaign.FetchGroupOffers(clients, groups, nil, 1.0, nil, bidding.StrategyCheap, 0.4)
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
		func(group campaign.InstanceGroup, phase string) {
			if cb.OnStatus != nil {
				cb.OnStatus(fmt.Sprintf("  %s: %s", group.GPUSpec(), phase))
			}
		},
		func(id int64) {
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
			warnings = append(warnings, fmt.Sprintf("Warning: reload job %d failed: %v", job.ID, err))
			continue
		}
		if latest == nil {
			warnings = append(warnings, fmt.Sprintf("Warning: job %d no longer exists, skipping", job.ID))
			continue
		}
		if latest.EffectiveStatus() != db.StatusQueued {
			warnings = append(warnings, fmt.Sprintf("Warning: job %d has status %s after unplace, skipping", job.ID, latest.EffectiveStatus()))
			continue
		}
		if latest.HasAssignedHost() {
			warnings = append(warnings, fmt.Sprintf("Warning: job %d is still placed after unplace, skipping", job.ID))
			continue
		}
		launchable = append(launchable, latest)
	}
	return launchable, warnings
}

func RefreshLaunchableJobs(database *sql.DB, jobs []*db.Job) ([]*db.Job, []string) {
	return refreshLaunchableJobs(database, jobs)
}

func countGroupJobs(groups []campaign.InstanceGroup) int {
	n := 0
	for _, g := range groups {
		n += len(g.Jobs)
	}
	return n
}
