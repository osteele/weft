package orchestration

import (
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/retrypolicy"
)

// RelaunchOrphanedJobs resets jobs on terminal cloud instances and launches
// replacement instances for scoped unplaced cloud jobs.
func RelaunchOrphanedJobs(
	database *sql.DB,
	cfg *config.Config,
	extraAttempts int,
	retryBudgetMultiplierByFailedInstance map[int64]float64,
	scopeJobIDs []int64,
	scopeProject string,
	restrictToReset bool,
	includeFreshUnplaced bool,
) (*campaign.RelaunchResult, error) {
	// Placement intents are now created per-group inside campaign.RelaunchOrphanedJobs,
	// scoped to jobs that are about to actually launch. Opening them upfront for the
	// entire scope misled the UI: skipped jobs (no offers, budget exhausted, etc.)
	// would briefly appear as "Placing" before being marked `confirmed: placement
	// succeeded` even though no launch happened.

	resetJobs, err := db.ResetJobsOnTerminalLaunches(database)
	if err != nil {
		slog.Warn("failed to reset jobs on terminal launches", "component", "auto-relaunch", "error", err)
	}

	if cfg == nil {
		cfg, _ = config.Load()
	}
	clients, err := BuildCloudClients(cfg)
	if err != nil {
		return nil, err
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("no cloud providers available")
	}

	overheadModel := buildOverheadModel(database)
	predCfg := buildPredictorConfig(cfg)
	minReliability := cfg.CampaignReliability()
	relaunchCfg := campaign.RelaunchConfig{
		Clients:               clients,
		R2Cfg:                 cfg.Vastai.R2.ToCloudR2Config(),
		CreateOptsForProvider: cfg.CloudCreateOpts,
		LaunchOpts:            campaign.LaunchOpts{GracePeriodSeconds: 15 * 60, GPUWarmup: cfg.Campaign.GPUWarmup},
		MaxAttempts:           retrypolicy.MaxAttemptsWithExtra(extraAttempts),
		SurvivalModel:         buildSurvivalModel(database),
		MinReliability:        &minReliability,
		MinSurvival:           0.4,
		Database:              database,
		AppConfig:             cfg,
		PredictorConfig:       &predCfg,
		ResetJobs:             resetJobs,
		RestrictToReset:       restrictToReset,
		IncludeFreshUnplaced:  includeFreshUnplaced,
		ScopeJobIDs:           scopeJobIDs,
		ScopeProject:          scopeProject,
		SetupFactory:          campaign.OfferSetupOverheadFactory(database, overheadModel),
		RetryBudget: &campaign.RetryBudget{
			FirstTimeLimit: cfg.RetryFirstTimeLimit(),
			FirstCostCents: cfg.RetryFirstCostLimitCents(),
			NextTimeLimit:  cfg.RetryNextTimeLimit(),
			NextCostCents:  cfg.RetryNextCostLimitCents(),
		},
		RunawayPolicy: &campaign.RunawayPolicy{
			Enabled:                             cfg.AutoRunawayEnabled(),
			Window:                              cfg.AutoRunawayWindow(),
			ChainNoProgressLimit:                cfg.AutoRunawayChainNoProgressLimit(),
			OrphanChurnLimit:                    cfg.AutoRunawayOrphanChurnLimit(),
			InfraFailureLimit:                   cfg.AutoRunawayInfraFailureLimit(),
			SpendNoProgressLimitCent:            cfg.AutoRunawaySpendNoProgressLimitCents(),
			AutoProbeInterval:                   campaign.DefaultAutoProbeInterval,
			AutoProbeIntervalAfterRecentSuccess: campaign.DefaultAutoProbeIntervalAfterRecentSuccess,
			AutoProbeRecentSuccessWindow:        campaign.DefaultAutoProbeRecentSuccessWindow,
		},
		RetryBudgetMultiplierByFailedInstance: retryBudgetMultiplierByFailedInstance,
	}

	// Auto-resume the breaker if a previously-launched probe completed
	// successfully. Cheap (one DB lookup, sometimes one launch fetch);
	// safe to run before every relaunch attempt.
	if err := campaign.MaybeAutoResumeBreaker(database); err != nil {
		slog.Warn("auto-probe resume check failed", "component", "auto-relaunch", "error", err)
	}

	result, err := campaign.RelaunchOrphanedJobs(relaunchCfg)
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = &campaign.RelaunchResult{}
	}

	// If the breaker just blocked this pass, schedule the next probe.
	// MaybeLaunchAutoProbe is rate-limited internally so it won't fire
	// more often than AutoProbeInterval.
	if result.BlockedReason != "" {
		if probeID, err := campaign.MaybeLaunchAutoProbe(relaunchCfg); err != nil {
			slog.Warn("auto-probe launch failed", "component", "auto-relaunch", "error", err)
		} else if probeID > 0 {
			slog.Info("auto-probe launched", "component", "auto-relaunch", "launch_id", probeID)
		}
	}
	return result, nil
}

func buildOverheadModel(database *sql.DB) *estimate.OverheadModel {
	obs, err := db.QueryOverheadObservations(database)
	if err != nil {
		slog.Warn("could not query overhead observations", "error", err)
		return nil
	}
	return estimate.BuildOverheadModel(obs)
}

func buildPredictorConfig(cfg *config.Config) predictor.Config {
	if cfg == nil {
		return predictor.Config{}
	}
	pcfg := predictor.BuildConfig(
		cfg.Predictor.ProjectPath,
		cfg.Predictor.ModelDir,
		cfg.Predictor.RetrainInterval,
		cfg.Predictor.DBPaths,
	)
	pcfg.Enabled = cfg.Predictor.Enabled
	return pcfg
}

func buildSurvivalModel(database *sql.DB) *bidding.SurvivalModel {
	outcomes, err := bidding.LoadInstanceOutcomes(database)
	if err != nil {
		slog.Warn("could not query instance outcomes", "error", err)
		return nil
	}
	return bidding.BuildSurvivalModel(outcomes)
}
