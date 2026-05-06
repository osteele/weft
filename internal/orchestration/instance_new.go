package orchestration

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/syncorch"
)

type NewInstanceOptions struct {
	JobScope            map[int64]struct{}
	Project             string
	Strategy            bidding.SelectionStrategy
	MinSurvival         float64
	DryRun              bool
	WaitReady           bool
	ReadyTimeout        time.Duration
	SyncInterval        time.Duration
	OnStatus            func(string)
	OnEvent             func(campaign.LaunchEvent)
	OnCampaign          func(campaignID int64)
	ConfirmBeforeLaunch func(NewInstanceResult) (bool, error)
	Clients             []cloud.Client
	R2Client            *r2.Client
	CreateOptions       func(cloud.Provider) (cloud.CreateOpts, error)
}

type NewInstanceResult struct {
	InstanceIDs []int64
	JobIDs      []int64
	AnchorJobID int64
	IntentIDs   []int64
	Offer       *cloud.Offer
	DryRun      bool
	Canceled    bool
	Warning     string
}

var launchCampaignForNewInstance = campaign.LaunchCampaign

func LaunchNewInstanceWithRebalance(ctx context.Context, database *sql.DB, cfg *config.Config, opts NewInstanceOptions) (NewInstanceResult, error) {
	if database == nil {
		return NewInstanceResult{}, fmt.Errorf("database is required")
	}
	if cfg == nil {
		var err error
		cfg, err = config.Load()
		if err != nil {
			return NewInstanceResult{}, fmt.Errorf("load config: %w", err)
		}
	}
	strategy := opts.Strategy
	if strategy == "" {
		strategy = bidding.StrategyFastest
	}
	minSurvival := opts.MinSurvival
	if minSurvival == 0 {
		minSurvival = 0.4
	}
	syncInterval := opts.SyncInterval
	if syncInterval <= 0 {
		syncInterval = 5 * time.Second
	}
	readyTimeout := opts.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = 20 * time.Minute
	}

	r2Client := opts.R2Client
	if r2Client == nil {
		var err error
		r2Client, err = BuildR2Client(cfg)
		if err != nil {
			return NewInstanceResult{}, fmt.Errorf("R2 client: %w", err)
		}
	}
	clients := opts.Clients
	if len(clients) == 0 {
		var err error
		clients, err = BuildCloudClients(cfg)
		if err != nil {
			return NewInstanceResult{}, fmt.Errorf("cloud providers: %w", err)
		}
	}
	if len(clients) == 0 {
		return NewInstanceResult{}, fmt.Errorf("no cloud providers available")
	}

	group, anchorID, err := planNewInstanceGroup(database, cfg, r2Client, opts.JobScope, opts.Project)
	if err != nil {
		return NewInstanceResult{}, err
	}
	groupOffers := campaign.FetchGroupOffers(clients, []campaign.InstanceGroup{group}, buildSurvivalModel(database), 1.0, nil, strategy, cfg.CampaignReliability(), minSurvival)
	if len(groupOffers) == 0 || groupOffers[0].Offer == nil {
		if len(groupOffers) > 0 && groupOffers[0].Err != nil {
			return NewInstanceResult{}, groupOffers[0].Err
		}
		return NewInstanceResult{}, fmt.Errorf("no compatible new-instance offer found for %s", group.GPUSpec())
	}
	offer := *groupOffers[0].Offer
	result := NewInstanceResult{
		JobIDs:      groupJobIDs(group),
		AnchorJobID: anchorID,
		Offer:       &offer,
		DryRun:      opts.DryRun,
	}
	if opts.DryRun {
		return result, nil
	}
	if opts.ConfirmBeforeLaunch != nil {
		confirmed, err := opts.ConfirmBeforeLaunch(result)
		if err != nil {
			return NewInstanceResult{}, err
		}
		if !confirmed {
			result.Canceled = true
			return result, nil
		}
	}

	intents, err := openNewInstanceMoveIntents(database, group.Jobs, &offer)
	if err != nil {
		return NewInstanceResult{}, err
	}
	for _, intent := range intents {
		result.IntentIDs = append(result.IntentIDs, intent.ID)
	}
	success := false
	defer func() {
		if success {
			return
		}
		for _, intent := range intents {
			_ = db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateCanceled, "new instance launch failed")
		}
	}()

	createOptions := opts.CreateOptions
	if createOptions == nil {
		createOptions = func(provider cloud.Provider) (cloud.CreateOpts, error) {
			return createOptsForProvider(cfg, provider)
		}
	}
	launchResult, err := launchCampaignForNewInstance(
		clients,
		database,
		[]campaign.InstanceGroup{group},
		[]cloud.Offer{offer},
		nil,
		buildSurvivalModel(database),
		campaign.LaunchOpts{
			GracePeriodSeconds: defaultGracePeriodSeconds(cfg),
			Strategy:           strategy,
			MinSurvival:        minSurvival,
			TransferClaim:      true,
		},
		cfg.Vastai.R2.ToCloudR2Config(),
		createOptions,
		opts.OnEvent,
		opts.OnCampaign,
		func(group campaign.InstanceGroup, instanceID int64) {
			for _, intent := range intents {
				_ = db.UpdateMoveIntentTargetLaunch(database, intent.ID, instanceID)
			}
		},
	)
	if err != nil {
		return result, err
	}
	if launchResult == nil || len(launchResult.InstanceIDs) == 0 {
		if launchResult != nil && len(launchResult.Errors) > 0 {
			return result, launchResult.Errors[0]
		}
		return result, fmt.Errorf("no instance launched")
	}
	result.InstanceIDs = launchResult.InstanceIDs
	success = true
	for _, e := range launchResult.Errors {
		if e != nil {
			result.Warning = appendWarning(result.Warning, e.Error())
		}
	}
	if !opts.WaitReady {
		for _, intent := range intents {
			_ = db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateConfirmed, "new instance launched without waiting for agent_ready")
		}
		return result, nil
	}
	readyID, err := waitForLaunchedInstanceReady(ctx, database, cfg, clients, r2Client, launchResult.InstanceIDs, readyTimeout, syncInterval, opts.OnStatus)
	if err != nil {
		return result, err
	}
	for _, intent := range intents {
		_ = db.ResolveMoveIntent(database, intent.ID, db.MoveIntentStateConfirmed, fmt.Sprintf("new instance %s accepted jobs", ids.FormatInstanceID(readyID)))
	}
	return result, nil
}

func planNewInstanceGroup(database *sql.DB, cfg *config.Config, r2Client *r2.Client, jobScope map[int64]struct{}, project string) (campaign.InstanceGroup, int64, error) {
	statuses := []string{db.StatusQueued, db.StatusPendingPlacement}
	jobs, err := db.ListJobsByStatuses(database, statuses, "", strings.TrimSpace(project), 0, nil, "")
	if err != nil {
		return campaign.InstanceGroup{}, 0, fmt.Errorf("list queued jobs: %w", err)
	}
	blocked, err := db.JobIDsWithOpenMoveOrPlacementIntents(database)
	if err != nil {
		return campaign.InstanceGroup{}, 0, err
	}
	candidates := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil || job.HasTag(db.TagInventory) {
			continue
		}
		if len(jobScope) > 0 {
			if _, ok := jobScope[job.ID]; !ok {
				continue
			}
		}
		if _, ok := blocked[job.ID]; ok {
			continue
		}
		candidates = append(candidates, job)
	}
	if len(candidates) == 0 {
		return campaign.InstanceGroup{}, 0, fmt.Errorf("no eligible queued jobs for new instance")
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return db.SchedulingLess(candidates[i], candidates[j])
	})
	selected := []*db.Job{candidates[0]}
	best, ok := prepareSingleNewInstanceGroup(database, cfg, r2Client, selected)
	if !ok {
		return campaign.InstanceGroup{}, 0, fmt.Errorf("anchor job %s cannot form a launch group", ids.FormatJobID(candidates[0].ID))
	}
	for _, candidate := range candidates[1:] {
		trial := append(append([]*db.Job(nil), selected...), candidate)
		group, ok := prepareSingleNewInstanceGroup(database, cfg, r2Client, trial)
		if !ok {
			continue
		}
		selected = trial
		best = group
	}
	return best, candidates[0].ID, nil
}

func prepareSingleNewInstanceGroup(database *sql.DB, cfg *config.Config, r2Client *r2.Client, jobs []*db.Job) (campaign.InstanceGroup, bool) {
	groups := campaign.PrepareGroupsWithConfig(jobsForGrouping(jobs), database, cfg, "", r2Client)
	if len(groups) != 1 || len(groups[0].Jobs) != len(jobs) {
		return campaign.InstanceGroup{}, false
	}
	return groups[0], true
}

func openNewInstanceMoveIntents(database *sql.DB, jobs []*db.Job, offer *cloud.Offer) ([]*db.MoveIntent, error) {
	intents := make([]*db.MoveIntent, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		var sourceLaunchID *int64
		if job.LaunchID != nil && *job.LaunchID > 0 {
			v := *job.LaunchID
			sourceLaunchID = &v
		}
		params := db.CreateMoveIntentParams{
			JobID:          job.ID,
			SourceLaunchID: sourceLaunchID,
			TargetKind:     db.MoveTargetNew,
		}
		if offer != nil {
			params.TargetOfferProvider = string(offer.Provider)
			params.TargetOfferID = offer.ProviderID
			params.TargetGPUName = offer.GPUName
		}
		intent, err := db.CreateMoveIntent(database, params)
		if err != nil {
			for _, opened := range intents {
				_ = db.ResolveMoveIntent(database, opened.ID, db.MoveIntentStateCanceled, "new instance planning failed")
			}
			return nil, fmt.Errorf("open move intent for job %s: %w", ids.FormatJobID(job.ID), err)
		}
		intents = append(intents, intent)
	}
	return intents, nil
}

func waitForLaunchedInstanceReady(
	ctx context.Context,
	database *sql.DB,
	cfg *config.Config,
	clients []cloud.Client,
	r2Client *r2.Client,
	instanceIDs []int64,
	timeout time.Duration,
	interval time.Duration,
	onStatus func(string),
) (int64, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for _, instanceID := range instanceIDs {
			launch, err := db.GetLaunch(database, instanceID)
			if err == nil && launch != nil {
				if launch.IsAgentReady() {
					return instanceID, nil
				}
				if campaign.IsInstanceTerminal(launch.Status) {
					return 0, fmt.Errorf("instance %s ended before agent_ready (%s)", ids.FormatInstanceID(instanceID), launch.Status)
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("timed out waiting for agent_ready on %s", formatInstanceList(instanceIDs))
		}
		if onStatus != nil {
			onStatus("Waiting for new instance agent_ready...")
		}
		syncorch.SyncCloud(cfg, database, syncorch.CloudSyncOptions{
			Context:    ctx,
			Timeout:    interval,
			Clients:    clientsAsAny(clients),
			R2Client:   r2Client,
			Reconciler: campaign.NewReconciler(),
		})
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}

func clientsAsAny(clients []cloud.Client) []any {
	out := make([]any, 0, len(clients))
	for _, client := range clients {
		out = append(out, client)
	}
	return out
}

func groupJobIDs(group campaign.InstanceGroup) []int64 {
	out := make([]int64, 0, len(group.Jobs))
	for _, job := range group.Jobs {
		if job != nil && job.ID > 0 {
			out = append(out, job.ID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func formatInstanceList(instanceIDs []int64) string {
	parts := make([]string, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		parts = append(parts, ids.FormatInstanceID(id))
	}
	return strings.Join(parts, ",")
}

func appendWarning(current, next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return current
	}
	if strings.TrimSpace(current) == "" {
		return next
	}
	return current + "; " + next
}

func defaultGracePeriodSeconds(cfg *config.Config) int {
	if cfg == nil {
		return int((5 * time.Minute).Seconds())
	}
	gracePeriod := cfg.DefaultGracePeriod()
	if gracePeriod == "" || gracePeriod == "0" {
		return 0
	}
	d, err := time.ParseDuration(gracePeriod)
	if err != nil {
		return int((5 * time.Minute).Seconds())
	}
	return int(d.Seconds())
}
