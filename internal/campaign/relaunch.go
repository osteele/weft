package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/retrypolicy"
	"github.com/osteele/weft/internal/runner"
)

// DefaultMaxCloudAttempts is the default maximum number of placement launch
// attempts per job before giving up. Keep this tied to retrypolicy so
// relaunches, move-to-new, and explicit new-instance launches use the same
// count budget.
var DefaultMaxCloudAttempts = retrypolicy.MaxPlacementAttempts()

// backoffEventEmitted tracks the last attempt count for which a
// relaunch.skipped.backoff event was recorded for each job, so the
// per-pass autopilot loop emits at most one event per (job, count) pair.
var backoffEventEmitted sync.Map

// runawayBlockedKey identifies a runaway breaker scope for blocked-event
// dedup.
type runawayBlockedKey struct {
	campaignID int64
	project    string
}

// runawayBlockedEventEmitted maps each runaway scope to the trip timestamp
// whose relaunch.runaway_blocked event has been recorded, so a tripped
// breaker writes one row per trip rather than one per pass. Process-local,
// like backoffEventEmitted: a restarted session re-emits once, which is
// bounded and preserves the audit trail.
var runawayBlockedEventEmitted sync.Map

// RelaunchConfig configures automatic relaunch of orphaned cloud jobs.
type RelaunchConfig struct {
	Clients               []cloud.Client
	R2Cfg                 cloud.R2Config
	CreateOpts            cloud.CreateOpts
	CreateOptsForProvider func(cloud.Provider) (cloud.CreateOpts, error)
	LaunchOpts            LaunchOpts
	MaxAttempts           int // default DefaultMaxCloudAttempts
	SurvivalModel         *bidding.SurvivalModel
	MinReliability        *float64 // nil uses default; 0 disables provider reliability filtering
	MinSurvival           float64  // 0 to disable survival filtering
	Strategy              bidding.SelectionStrategy
	Database              *sql.DB
	AppConfig             *config.Config
	PredictorConfig       *predictor.Config
	ResetJobs             map[int64]int64      // jobID → failed instanceID
	RestrictToReset       bool                 // when true and ResetJobs is non-empty, only relaunch reset jobs
	ScopeJobIDs           []int64              // optional job scope (current watch/project queue)
	ScopeProject          string               // optional project scope label for safety events
	SetupFactory          SetupOverheadFactory // per-offer setup time estimator; use OfferSetupOverheadFactory to build
	RetryBudget           *RetryBudget         // optional hard stop limits for retry instances
	RunawayPolicy         *RunawayPolicy       // optional unattended runaway breaker
	// RetryBudgetMultiplierByFailedInstance scales the applicable retry-budget
	// tier (first vs subsequent) for jobs that were orphaned from a specific
	// failed instance. Used by watch-mode budget raise.
	RetryBudgetMultiplierByFailedInstance map[int64]float64
	// IncludeFreshUnplaced allows relaunching scoped unplaced jobs even when
	// they have no prior cloud attempts and no rental tag.
	IncludeFreshUnplaced bool
	// BypassRunawayBreaker skips the runaway-breaker trip check for this
	// pass. Used by MaybeLaunchAutoProbe to fire a single probe launch
	// even when the breaker is tripped. Should not be set for normal
	// relaunch traffic.
	BypassRunawayBreaker bool
	// OnInstanceLaunched is called after a replacement launch is registered.
	// It lets higher-level durable workflows (for example move-to-new intents)
	// attach their own state to the new launch without forking relaunch logic.
	OnInstanceLaunched func(group InstanceGroup, instanceID int64)
}

// RetryBudget defines hard retry-stop limits by retry tier.
// "First" applies to the first retry attempt for a job; "Next" applies to
// second and subsequent retries.
type RetryBudget struct {
	FirstTimeLimit time.Duration
	FirstCostCents int
	NextTimeLimit  time.Duration
	NextCostCents  int
}

// RunawayPolicy defines guardrails for unattended relaunch loops.
type RunawayPolicy struct {
	Enabled                  bool
	Window                   time.Duration
	ChainNoProgressLimit     int
	OrphanChurnLimit         int
	InfraFailureLimit        int
	SpendNoProgressLimitCent int
	ResumeGracePeriod        time.Duration // skip breaker check for this long after a resume
	// AutoProbeInterval controls how often the autopilot launches a single
	// probe instance while the breaker is tripped. 0 disables auto-probing.
	// On success the breaker auto-resumes; on failure it stays tripped and
	// the next probe waits another interval. Bounds: probe cost = ~$0.05
	// per attempt; default interval is 1h so worst-case sustained cost
	// during an outage is ~$1.20/day.
	AutoProbeInterval time.Duration
	// AutoProbeIntervalAfterRecentSuccess shortens the auto-probe cadence
	// when the breaker re-trips shortly after a successful auto-resume:
	// recent success is evidence the provider is partially working, so
	// we should retry sooner than the cold-start cadence. Activates when
	// the most recent EventRelaunchAutoProbeResumed is within
	// AutoProbeRecentSuccessWindow of now. 0 falls back to AutoProbeInterval.
	AutoProbeIntervalAfterRecentSuccess time.Duration
	// AutoProbeRecentSuccessWindow is the lookback for treating a prior
	// auto-resume as "recent." 0 disables the tight-cadence path.
	AutoProbeRecentSuccessWindow time.Duration
}

// RelaunchResult holds the outcome of a relaunch pass.
type RelaunchResult struct {
	InstanceIDs   []int64 // newly launched instance DB IDs
	Skipped       int     // jobs skipped (max attempts + budget limits + no offers)
	BudgetSkip    int     // jobs skipped due to retry budget limits
	BlockedReason string  // non-empty when relaunch is blocked by runaway breaker
	Errors        []error
	// NotReplacedReasons explains why orphaned jobs from a failed instance were
	// not relaunched on this pass.
	NotReplacedReasons map[int64]string // failed instance ID -> reason
	// BudgetBlocked marks failed instances whose orphaned jobs were skipped due
	// to retry budget limits.
	BudgetBlocked map[int64]bool // failed instance ID -> true
	// JobReasons explains why specific jobs were skipped this pass, keyed by
	// job ID. Unlike NotReplacedReasons (keyed by failed instance ID), this
	// covers fresh unplaced jobs with no prior launch.
	JobReasons map[int64]string // job ID -> reason
}

// RelaunchOrphanedJobs finds unplaced cloud jobs, filters by attempt count,
// groups them, fetches offers, and launches new instances. It reuses the
// campaign from the most recent attempt (if any) for cost inheritance.
func RelaunchOrphanedJobs(cfg RelaunchConfig) (rr *RelaunchResult, rerr error) {
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxCloudAttempts
	}
	// Write a pass-summary lifecycle event so the autopilot's "was anything
	// actually attempted this pass?" question is answerable from the DB.
	// Without this, silent branches (e.g. groupOffers empty, or launch
	// goroutines with a nil client) leave no trace beyond relaunch.eligible.
	// No-op passes (nothing eligible, launched, skipped, or errored) write
	// nothing: a per-pass row at autopilot cadence records no information.
	var eligibleCount int
	defer func() {
		if rr == nil {
			return
		}
		if eligibleCount == 0 && len(rr.InstanceIDs) == 0 && rr.Skipped == 0 && len(rr.Errors) == 0 {
			return
		}
		_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
			EventKind: db.EventRelaunchPassSummary,
			JobCount:  eligibleCount,
			Detail: fmt.Sprintf(
				"eligible=%d launched=%d skipped=%d errors=%d blocked=%q",
				eligibleCount, len(rr.InstanceIDs), rr.Skipped, len(rr.Errors), rr.BlockedReason,
			),
		})
	}()

	unplaced, err := db.ListUnplacedJobs(cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("list unplaced jobs: %w", err)
	}

	// Optionally scope relaunch to the current watch/project queue.
	if len(cfg.ScopeJobIDs) > 0 {
		inScope := make(map[int64]struct{}, len(cfg.ScopeJobIDs))
		for _, id := range cfg.ScopeJobIDs {
			inScope[id] = struct{}{}
		}
		filtered := make([]*db.Job, 0, len(unplaced))
		for _, j := range unplaced {
			if _, ok := inScope[j.ID]; ok {
				filtered = append(filtered, j)
			}
		}
		unplaced = filtered
	}

	// If we know exactly which jobs were just orphaned and this pass is in
	// retry-only mode, restrict to those jobs.
	if cfg.RestrictToReset && len(cfg.ResetJobs) > 0 {
		filtered := make([]*db.Job, 0, len(cfg.ResetJobs))
		for _, j := range unplaced {
			if _, ok := cfg.ResetJobs[j.ID]; ok {
				filtered = append(filtered, j)
			}
		}
		unplaced = filtered
	}

	// Single pass: filter to cloud jobs and check attempt count.
	// Attempts are scoped to the job's current campaign so that prior
	// campaigns (earlier `launch` commands) don't exhaust the retry budget.
	result := &RelaunchResult{
		NotReplacedReasons: map[int64]string{},
		BudgetBlocked:      map[int64]bool{},
		JobReasons:         map[int64]string{},
	}

	// Reuse pass: place unplaced jobs onto already-running cloud instances
	// when a compatible host exists. The runaway breaker only gates *new*
	// instance creation (which is the source of churn the breaker exists to
	// stop), so this pass runs unconditionally — a tripped breaker must not
	// strand jobs whose --needs producer is already running on a paid-up
	// instance. After this pass, `unplaced` retains only jobs that still
	// require a fresh instance.
	unplaced = tryPlaceOntoExistingInstances(cfg, unplaced, maxAttempts)

	if cfg.RunawayPolicy != nil && cfg.RunawayPolicy.Enabled && !cfg.BypassRunawayBreaker {
		if blocked, reason, err := evaluateRunawayBreaker(cfg.Database, cfg, unplaced, time.Now()); err != nil {
			slog.Warn("runaway breaker evaluation failed", "component", "relaunch", "error", err)
		} else if blocked {
			// Provider-pinned jobs bypass a global trip when their provider
			// has zero failures on the trip — a Vast outage shouldn't pause RunPod.
			survivors := unplacedForHealthyProviders(cfg.Database, unplaced)
			if len(survivors) == 0 {
				result.BlockedReason = reason
				return result, nil
			}
			slog.Info("runaway tripped but routing survivors via healthy providers",
				"component", "relaunch", "tripped_reason", reason, "survivor_jobs", len(survivors))
			unplaced = survivors
		}
	}
	var eligible []*db.Job
	for _, j := range unplaced {
		if j.HasTag(db.TagInventory) {
			continue
		}
		facts, err := attemptFactsForRelaunch(cfg.Database, j.ID)
		if err != nil {
			result.Skipped++
			recordJobSkipReason(result, j.ID, 0, "attempt history unavailable: "+err.Error())
			continue
		}
		count := facts.Count
		if count == 0 && !j.HasTag(db.TagRental) && !cfg.IncludeFreshUnplaced {
			continue
		}
		failedInstanceID := failedInstanceForJob(j.ID, facts.LastLaunch, cfg.ResetJobs)
		if count >= maxAttempts {
			slog.Debug("job exceeds max attempts, skipping", "component", "relaunch", "job_id", j.ID, "attempts", count, "max_attempts", maxAttempts)
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind:     db.EventRelaunchSkippedMaxAttempts,
				JobID:         j.ID,
				GPUSpec:       j.GPUClass,
				AttemptNumber: count,
				MaxAttempts:   maxAttempts,
			})
			result.Skipped++
			recordJobSkipReason(result, j.ID, failedInstanceID, "max cloud attempts reached")
			continue
		}
		if count > 0 {
			if remaining := backoffRemaining(facts, time.Now()); remaining > 0 {
				detail := fmt.Sprintf("backoff %s remaining (after %d failure(s))", remaining.Truncate(time.Second), count)
				slog.Debug("job in backoff window, skipping",
					"component", "relaunch",
					"job_id", j.ID,
					"remaining", remaining.Truncate(time.Second),
					"attempts", count)
				if prev, ok := backoffEventEmitted.Load(j.ID); !ok || prev.(int) != count {
					_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
						EventKind:     db.EventRelaunchSkippedBackoff,
						JobID:         j.ID,
						GPUSpec:       j.GPUClass,
						AttemptNumber: count,
						Detail:        detail,
					})
					backoffEventEmitted.Store(j.ID, count)
				}
				result.Skipped++
				recordJobSkipReason(result, j.ID, failedInstanceID, detail)
				continue
			}
			backoffEventEmitted.Delete(j.ID)
		}
		if cfg.RetryBudget != nil && count > 0 {
			budget := applyRetryBudgetMultiplier(*cfg.RetryBudget, count, budgetMultiplierForFailedInstance(cfg, failedInstanceID))
			elapsed, spendCents := retryBudgetUsage(facts, time.Now())
			if exceeded, detail := exceedsRetryBudget(budget, count, elapsed, spendCents); exceeded {
				slog.Info("job exceeds retry budget, skipping",
					"component", "relaunch",
					"job_id", j.ID,
					"attempts", count,
					"elapsed", elapsed.Truncate(time.Second),
					"spend_cents", spendCents,
					"detail", detail)
				_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
					EventKind:     db.EventRelaunchSkippedMaxAttempts,
					JobID:         j.ID,
					GPUSpec:       j.GPUClass,
					AttemptNumber: count,
					MaxAttempts:   maxAttempts,
					Detail:        detail,
				})
				result.Skipped++
				result.BudgetSkip++
				recordJobSkipReason(result, j.ID, failedInstanceID, summarizeBudgetDetail(detail))
				if failedInstanceID != 0 {
					result.BudgetBlocked[failedInstanceID] = true
				}
				continue
			}
		}
		if reason, blocked := queueblock.WaitingOnJobDependencyReason(cfg.Database, j); blocked {
			slog.Debug("job waiting on dependency, skipping",
				"component", "relaunch",
				"job_id", j.ID,
				"reason", reason)
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchSkippedWaitingOnProducer,
				JobID:     j.ID,
				GPUSpec:   j.GPUClass,
				Detail:    reason,
			})
			result.Skipped++
			recordJobSkipReason(result, j.ID, failedInstanceID, reason)
			continue
		}
		// Pre-flight: skip consumers whose --needs producers aren't ready
		// (avoids guaranteed 404s in ResolveSpecs and the resulting retry loop).
		if reason, blocked := queueblock.WaitingOnProducerReason(cfg.Database, j); blocked {
			slog.Debug("job waiting on producer, skipping",
				"component", "relaunch",
				"job_id", j.ID,
				"reason", reason)
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchSkippedWaitingOnProducer,
				JobID:     j.ID,
				GPUSpec:   j.GPUClass,
				Detail:    reason,
			})
			result.Skipped++
			recordJobSkipReason(result, j.ID, failedInstanceID, reason)
			continue
		}
		eligible = append(eligible, j)
	}

	if len(eligible) == 0 {
		return result, nil
	}

	eligibleCount = len(eligible)
	slog.Info("eligible orphaned jobs for relaunch", "component", "relaunch", "count", eligibleCount)
	_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchEligible,
		JobCount:  eligibleCount,
	})

	// Group by GPU requirements and estimate disk.
	// Auto-relaunch can transiently mark scoped jobs as pending_placement to
	// prevent duplicate submissions while a pass is in flight. Treat those
	// unplaced jobs as queued for grouping so offer search and blocked-reason
	// reporting still run for the pass.
	groups := GroupByAffinity(relaunchGroupingJobs(eligible), nil)
	groups = SplitGroupsByImage(cfg.Database, groups)
	groups = ApplyImageMetadataRequirements(cfg.AppConfig, groups)
	ApplyBidLossEscalation(cfg.Database, groups)
	var r2Client *r2.Client
	if cfg.R2Cfg.Bucket != "" && cfg.R2Cfg.AccessKeyID != "" {
		var err error
		r2Client, err = r2.New(r2.Config{
			AccountID:       cfg.R2Cfg.AccountID,
			AccessKeyID:     cfg.R2Cfg.AccessKeyID,
			SecretAccessKey: cfg.R2Cfg.SecretAccessKey,
			Bucket:          cfg.R2Cfg.Bucket,
		})
		if err != nil {
			slog.Warn("failed to build R2 client for disk estimation", "component", "relaunch", "error", err)
		}
	}
	groups = EstimateGroupDisks(groups, cfg.Database, r2Client)

	// If the most recent attempt for a group failed with disk_full, bump the
	// disk allocation so we don't retry a failing configuration. This only
	// applies within the automatic relaunch path — manual relaunches re-estimate
	// from scratch (the user may have edited the job or sources).
	for i, group := range groups {
		priorDisk := mostRecentDiskFullGB(cfg.Database, group)
		if priorDisk > 0 {
			floor := priorDisk + priorDisk/2 // 1.5x
			if capGB := groupDiskCapGB(group); capGB > 0 && floor > capGB {
				slog.Warn("disk_full bump limited by user-declared ceiling",
					"component", "relaunch",
					"gpu_spec", group.GPUSpec(),
					"wanted_gb", floor,
					"cap_gb", capGB,
					"prior_disk_full_gb", priorDisk)
				floor = capGB
			}
			if groups[i].DiskGB < floor {
				slog.Info("raising disk allocation due to prior disk_full", "component", "relaunch", "gpu_spec", group.GPUSpec(), "from_gb", groups[i].DiskGB, "to_gb", floor, "prior_disk_full_gb", priorDisk)
				_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
					EventKind: db.EventRelaunchDiskBump,
					GPUSpec:   group.GPUSpec(),
					DiskGB:    int(floor),
					Detail:    fmt.Sprintf("raised from %dGB (prior disk_full at %dGB)", groups[i].DiskGB, priorDisk),
				})
				groups[i].DiskGB = floor
			}
		}
	}

	// Fetch offers
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = bidding.StrategyCheap
	}
	minReliability := 0.95
	if cfg.MinReliability != nil {
		if *cfg.MinReliability >= 0 && *cfg.MinReliability <= 1 {
			minReliability = *cfg.MinReliability
		}
	}
	rawOffers := FetchGroupRawOffers(cfg.Clients, groups, minReliability)
	driverExclusionSummaries := applyDriverFailureExclusions(cfg.Database, r2Client, rawOffers)
	groupOffers := RankGroupOffersWithPredictor(rawOffers, cfg.PredictorConfig, cfg.SurvivalModel, cfg.SetupFactory, strategy, cfg.MinSurvival)
	RecordOfferAvailabilitySnapshots(cfg.Database, rawOffers, groupOffers, minReliability)

	// Filter to groups with valid offers
	var launchGroups []InstanceGroup
	var launchOffers []cloud.Offer
	for i, gOffer := range groupOffers {
		// Err must be checked before Offer == nil: a search error always leaves
		// Offer nil, so the inverse order would mis-classify provider failures
		// as "no offers" (with a misleading FilterStats-derived detail).
		if gOffer.Err != nil {
			slog.Warn("offer error for group", "component", "relaunch", "gpu_spec", gOffer.Group.GPUSpec(), "error", gOffer.Err)
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchSkippedOfferError,
				GPUSpec:   gOffer.Group.GPUSpec(),
				ErrorText: gOffer.Err.Error(),
			})
			result.Errors = append(result.Errors, gOffer.Err)
			recordGroupReasons(result, cfg.ResetJobs, gOffer.Group, "offer query failed: "+gOffer.Err.Error())
			continue
		}
		if gOffer.Offer == nil {
			constraintStr := FormatOfferConstraints(offerConstraintsForGroup(gOffer.Group, minReliability))
			detail := gOffer.FilterStats.NoOffersDetail(constraintStr)
			if i < len(driverExclusionSummaries) {
				detail = appendDriverFailureExclusionDetail(detail, driverExclusionSummaries[i])
			}
			slog.Warn("no offers for group, skipping", "component", "relaunch", "gpu_spec", gOffer.Group.GPUSpec(), "job_count", len(gOffer.Group.Jobs), "detail", detail)
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchSkippedNoOffers,
				GPUSpec:   gOffer.Group.GPUSpec(),
				JobCount:  len(gOffer.Group.Jobs),
				Detail:    detail,
			})
			result.Skipped += len(gOffer.Group.Jobs)
			recordGroupReasons(result, cfg.ResetJobs, gOffer.Group, detail)
			continue
		}
		launchGroups = append(launchGroups, gOffer.Group)
		launchOffers = append(launchOffers, *gOffer.Offer)
	}

	if len(launchGroups) == 0 {
		return result, nil
	}

	// Prepare R2 assets (agent binary + source tarballs). Per-source-dir
	// upload failures (e.g., one project exceeds the source-tarball size
	// limit) must not poison sibling groups in the same pass — they get
	// recorded against the failing group's jobs and the surviving groups
	// proceed. The outer error is reserved for fatal failures (agent
	// upload, stager init) where no launch is possible.
	r2Assets, perDirErr, err := PrepareR2AssetsPerDirWithReporter(
		cfg.R2Cfg,
		launchGroups,
		func(status AssetStageStatus) {
			recordRelaunchAssetStageEvent(cfg.Database, launchGroups, status)
		},
	)
	if err != nil {
		return result, fmt.Errorf("prepare R2 assets: %w", err)
	}
	if len(perDirErr) > 0 {
		survivingGroups := launchGroups[:0]
		survivingOffers := launchOffers[:0]
		for i, group := range launchGroups {
			groupErr := groupSourceUploadError(group, perDirErr)
			if groupErr == nil {
				survivingGroups = append(survivingGroups, group)
				survivingOffers = append(survivingOffers, launchOffers[i])
				continue
			}
			reason := fmt.Sprintf("prepare R2 assets: %s", groupErr.Error())
			slog.Warn("skipping group: R2 source upload failed",
				"component", "relaunch",
				"gpu_spec", group.GPUSpec(),
				"job_count", len(group.Jobs),
				"error", groupErr)
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchSkippedNoOffers,
				GPUSpec:   group.GPUSpec(),
				JobCount:  len(group.Jobs),
				Detail:    reason,
			})
			result.Skipped += len(group.Jobs)
			recordGroupReasons(result, cfg.ResetJobs, group, reason)
		}
		launchGroups = survivingGroups
		launchOffers = survivingOffers
		if len(launchGroups) == 0 {
			return result, nil
		}
	}

	// Determine campaign ID from most recent attempt
	var campaignID *int64
	for _, j := range eligible {
		attempts, err := db.GetLaunchAttempts(cfg.Database, j.ID)
		if err != nil || len(attempts) == 0 {
			continue
		}
		lastAttempt := attempts[len(attempts)-1]
		ci, err := db.GetLaunch(cfg.Database, lastAttempt.LaunchID)
		if err != nil || ci == nil || ci.CampaignID == nil {
			continue
		}
		campaignID = ci.CampaignID
		break
	}

	// Build a map from group index to the most recent failed instance ID,
	// so we can record it as replaced_instance_id on the new instance.
	groupPredecessorIDs := make(map[int]int64)
	for i, group := range launchGroups {
		if len(cfg.ResetJobs) > 0 {
			// Use the known failed instance ID from the reset pass.
			// Pick the most common instance in the group for mixed groups.
			counts := make(map[int64]int)
			for _, j := range group.Jobs {
				if instID, ok := cfg.ResetJobs[j.ID]; ok {
					counts[instID]++
				}
			}
			var bestID int64
			var bestCount int
			for id, c := range counts {
				if c > bestCount {
					bestID = id
					bestCount = c
				}
			}
			if bestCount > 0 {
				groupPredecessorIDs[i] = bestID
			}
		} else {
			// Fallback for callers that don't provide ResetJobs.
			for _, j := range group.Jobs {
				attempts, err := db.GetLaunchAttempts(cfg.Database, j.ID)
				if err != nil || len(attempts) == 0 {
					continue
				}
				lastAttempt := attempts[len(attempts)-1]
				groupPredecessorIDs[i] = lastAttempt.LaunchID
				break
			}
		}
	}

	// Launch instances in parallel
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, group := range launchGroups {
		offer := launchOffers[i]
		client := clientForProvider(cfg.Clients, offer.Provider)
		if client == nil {
			reason := fmt.Sprintf("no cloud client configured for provider %q (offer selected from a provider we can no longer reach)", offer.Provider)
			slog.Warn("skipping group: no client for provider",
				"component", "relaunch",
				"gpu_spec", group.GPUSpec(),
				"provider", offer.Provider,
				"job_count", len(group.Jobs))
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchSkippedNoClient,
				GPUSpec:   group.GPUSpec(),
				JobCount:  len(group.Jobs),
				Detail:    fmt.Sprintf("provider=%s", offer.Provider),
			})
			mu.Lock()
			result.Errors = append(result.Errors, fmt.Errorf("relaunch: no client for provider %s", offer.Provider))
			result.Skipped += len(group.Jobs)
			recordGroupReasons(result, cfg.ResetJobs, group, reason)
			mu.Unlock()
			continue
		}

		predecessorID, hasPredecessor := groupPredecessorIDs[i]
		wg.Add(1)
		go func(group InstanceGroup, offer cloud.Offer, client cloud.Client, predecessorID int64, hasPredecessor bool) {
			defer wg.Done()
			initialCreateOpts, createOptsErr := resolveRelaunchCreateOpts(cfg, offer.Provider)
			if createOptsErr != nil {
				mu.Lock()
				defer mu.Unlock()
				reason := fmt.Sprintf("resolve create opts failed: %v", createOptsErr)
				slog.Warn("launch skipped for group", "component", "relaunch", "gpu_spec", group.GPUSpec(), "provider", offer.Provider, "error", createOptsErr)
				_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
					EventKind: db.EventRelaunchSkippedNoClient,
					GPUSpec:   group.GPUSpec(),
					JobCount:  len(group.Jobs),
					Detail:    reason,
					ErrorText: createOptsErr.Error(),
				})
				result.Errors = append(result.Errors, fmt.Errorf("%s: %w", group.GPUSpec(), createOptsErr))
				recordGroupReasons(result, cfg.ResetJobs, group, reason)
				return
			}
			createOptsForOffer := func(o cloud.Offer) (cloud.CreateOpts, error) {
				if o.Provider == offer.Provider {
					return initialCreateOpts, nil
				}
				return resolveRelaunchCreateOpts(cfg, o.Provider)
			}
			// Open placement intents for this group's jobs immediately before
			// LaunchInstance, so the autopilot/UI sees them as Placing only
			// during the actual launch attempt. Skipped groups (no offers,
			// no client, etc.) never open intents and remain Unplaced with
			// their existing block reason.
			intentIDs := openRelaunchIntents(cfg.Database, group.Jobs)
			emittedRunpodSSHWaiting := false
			progress := cloud.ProgressFunc(func(phase string) {
				if emittedRunpodSSHWaiting || runpodObservedBootstrapPhase(phase) != runpodSSHWaitingPhase {
					return
				}
				emittedRunpodSSHWaiting = true
				_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
					EventKind: db.EventRelaunchRunpodSSHWaiting,
					GPUSpec:   group.GPUSpec(),
					JobCount:  len(group.Jobs),
					Detail:    runpodSSHWaitingPhase,
				})
			})
			replans := retrypolicy.MaxGroupReplans()
			if cfg.LaunchOpts.DistinctMachines || (offer.Provider == cloud.ProviderRunpod && group.MinDriverVersion > 0) {
				replans += retrypolicy.MaxCreateAttempts() - 1
			}
			var blockers []runpodHeldBlocker
			instanceID, _, _, err := runGroupLaunchWithReplan(
				offer,
				client,
				replans,
				func(c cloud.Client, o cloud.Offer) (int64, error) {
					createOpts, optsErr := createOptsForOffer(o)
					if optsErr != nil {
						return 0, optsErr
					}
					launchOpts := cfg.LaunchOpts
					launchOpts.holdRunpodDriverBlockers = true
					id, launchErr := LaunchInstance(
						c, cfg.Clients, cfg.Database, campaignID, group, o,
						launchOpts, cfg.R2Cfg, createOpts,
						*r2Assets, nil, progress, nil,
					)
					var conflictErr *distinctMachinePostCreateConflictError
					if errors.As(launchErr, &conflictErr) && conflictErr.KeepAlive {
						blockers = append(blockers, runpodHeldBlocker{
							LaunchID:   conflictErr.LaunchID,
							Provider:   conflictErr.Provider,
							ProviderID: conflictErr.ProviderID,
						})
					}
					var driverErr *postCreateDriverCompatibilityError
					if errors.As(launchErr, &driverErr) && driverErr.KeepAlive {
						blockers = append(blockers, runpodHeldBlocker{
							LaunchID:   driverErr.LaunchID,
							Provider:   driverErr.Provider,
							ProviderID: driverErr.ProviderID,
						})
					}
					return id, launchErr
				},
				func() (*cloud.Offer, error) {
					return relaunchFreshOffer(cfg, group, minReliability, strategy, r2Assets.Client)
				},
				func(p cloud.Provider) cloud.Client {
					return clientForProvider(cfg.Clients, p)
				},
				progress,
			)
			blockerDetail := "replacement accepted; releasing held RunPod pod"
			if err != nil {
				blockerDetail = "launch failed; releasing held RunPod pod"
			}
			destroyRunpodHeldBlockers(cfg.Database, func(p cloud.Provider) cloud.Client {
				return clientForProvider(cfg.Clients, p)
			}, blockers, blockerDetail)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				closeRelaunchIntents(cfg.Database, intentIDs, false, err.Error())
				slog.Warn("launch failed for group", "component", "relaunch", "gpu_spec", group.GPUSpec(), "error", err)
				_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
					EventKind: db.EventRelaunchLaunchFailed,
					GPUSpec:   group.GPUSpec(),
					JobCount:  len(group.Jobs),
					ErrorText: err.Error(),
				})
				result.Errors = append(result.Errors, fmt.Errorf("%s: %w", group.GPUSpec(), err))
				recordGroupReasons(result, cfg.ResetJobs, group, err.Error())
				return
			}
			closeRelaunchIntents(cfg.Database, intentIDs, true, "placement succeeded")
			if hasPredecessor {
				if setErr := db.SetLaunchReplacedID(cfg.Database, instanceID, predecessorID); setErr != nil {
					slog.Warn("failed to set replaced_instance_id", "component", "relaunch", "instance", instanceID, "error", setErr)
				}
				delete(result.NotReplacedReasons, predecessorID)
				delete(result.BudgetBlocked, predecessorID)
			}
			slog.Info("launched instance for relaunch", "component", "relaunch", "instance", instanceID, "job_count", len(group.Jobs), "gpu_spec", group.GPUSpec())
			if cfg.OnInstanceLaunched != nil {
				cfg.OnInstanceLaunched(group, instanceID)
			}
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchLaunchSuccess,
				LaunchID:  instanceID,
				GPUSpec:   group.GPUSpec(),
				JobCount:  len(group.Jobs),
			})
			result.InstanceIDs = append(result.InstanceIDs, instanceID)
			launchHedgeProbes(cfg, campaignID, group, offer, *r2Assets, instanceID, &mu, result)
		}(group, offer, client, predecessorID, hasPredecessor)
	}
	wg.Wait()

	return result, nil
}

func recordRelaunchAssetStageEvent(database *sql.DB, groups []InstanceGroup, status AssetStageStatus) {
	if status.Phase == "" {
		return
	}
	gpuSpec, jobCount := assetStageGroupSummary(groups)
	event := &db.LifecycleEvent{
		EventKind: db.EventRelaunchAssetStage,
		GPUSpec:   gpuSpec,
		JobCount:  jobCount,
		Detail:    assetStageEventDetail(status),
	}
	if status.Err != nil {
		event.ErrorText = status.Err.Error()
	}
	if err := db.InsertLifecycleEvent(database, event); err != nil {
		slog.Debug("record relaunch asset stage event", "component", "relaunch", "phase", status.Phase, "error", err)
	}
}

func assetStageGroupSummary(groups []InstanceGroup) (string, int) {
	specs := make(map[string]struct{})
	jobCount := 0
	for _, group := range groups {
		if spec := group.GPUSpec(); spec != "" {
			specs[spec] = struct{}{}
		}
		jobCount += len(group.Jobs)
	}
	if len(specs) == 1 {
		for spec := range specs {
			return spec, jobCount
		}
	}
	return "", jobCount
}

func assetStageEventDetail(status AssetStageStatus) string {
	label := status.Label
	if label == "" {
		label = status.Key
	}
	if label == "" {
		return fmt.Sprintf("%s %s", status.Kind, status.Phase)
	}
	return fmt.Sprintf("%s %s %s", status.Kind, label, status.Phase)
}

// launchHedgeProbes spawns probe instances for the primary's hedge
// cohort. Probe launches run in a background goroutine and tolerate
// individual failures — they're fire-and-forget; the cull rule
// enforces single-survivor.
func launchHedgeProbes(cfg RelaunchConfig, campaignID *int64, group InstanceGroup, primaryOffer cloud.Offer, r2Assets R2Assets, primaryInstanceID int64, mu *sync.Mutex, result *RelaunchResult) {
	if cfg.AppConfig == nil {
		return
	}
	hedge := cfg.AppConfig.Campaign.Hedge
	if !hedge.Enabled || hedge.Count <= 1 {
		return
	}
	if hedge.MaxCostPerHour > 0 && primaryOffer.CostPerHour > hedge.MaxCostPerHour {
		return
	}
	if err := db.SetLaunchHedgeCohort(cfg.Database, primaryInstanceID, primaryInstanceID); err != nil {
		slog.Warn("set hedge cohort id on primary", "component", "relaunch", "instance", primaryInstanceID, "error", err)
		return
	}

	probeCount := hedge.Count - 1
	go func() {
		var setupOverhead bidding.OfferSetupFunc
		if cfg.SetupFactory != nil {
			setupOverhead = cfg.SetupFactory(group)
		}
		offers := SearchTopKOffersForGroup(
			cfg.Clients, group, probeCount, cfg.SurvivalModel, 0, setupOverhead,
			map[string]struct{}{primaryOffer.Key(): {}},
			cfg.Strategy, cfg.AppConfig.CampaignReliability(), cfg.MinSurvival,
		)
		for _, gOffer := range offers {
			probeOffer := *gOffer.Offer
			if hedge.MaxCostPerHour > 0 && probeOffer.CostPerHour > hedge.MaxCostPerHour {
				return
			}
			probeClient, probeCreateOpts, targetErr := resolveHedgeProbeLaunchTarget(cfg, probeOffer)
			if targetErr != nil {
				slog.Warn("hedge probe launch target unavailable", "component", "relaunch", "primary", primaryInstanceID, "provider", probeOffer.Provider, "error", targetErr)
				continue
			}
			probeOpts := cfg.LaunchOpts
			probeOpts.HedgeProbe = true
			probeOpts.HedgeCohortID = primaryInstanceID
			probeID, err := LaunchInstance(probeClient, nil, cfg.Database, campaignID, group, probeOffer, probeOpts, cfg.R2Cfg, probeCreateOpts, r2Assets, nil, nil, nil)
			if err != nil {
				slog.Warn("hedge probe launch failed", "component", "relaunch", "primary", primaryInstanceID, "error", err)
				continue
			}
			slog.Info("hedge probe launched", "component", "relaunch", "primary", primaryInstanceID, "probe", probeID, "machine", probeOffer.MachineID)
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchLaunchSuccess,
				LaunchID:  probeID,
				GPUSpec:   group.GPUSpec(),
				JobCount:  0,
				Detail:    fmt.Sprintf("hedge probe of primary=%d", primaryInstanceID),
			})
			mu.Lock()
			result.InstanceIDs = append(result.InstanceIDs, probeID)
			mu.Unlock()
		}
	}()
}

func resolveHedgeProbeLaunchTarget(cfg RelaunchConfig, offer cloud.Offer) (cloud.Client, cloud.CreateOpts, error) {
	client := clientForProvider(cfg.Clients, offer.Provider)
	if client == nil {
		return nil, cloud.CreateOpts{}, fmt.Errorf("no cloud client configured for provider %q", offer.Provider)
	}
	createOpts, err := resolveRelaunchCreateOpts(cfg, offer.Provider)
	if err != nil {
		return nil, cloud.CreateOpts{}, err
	}
	return client, createOpts, nil
}

func resolveRelaunchCreateOpts(cfg RelaunchConfig, provider cloud.Provider) (cloud.CreateOpts, error) {
	if cfg.CreateOptsForProvider != nil {
		return cfg.CreateOptsForProvider(provider)
	}
	createOpts := cfg.CreateOpts
	if provider == cloud.ProviderVastai && strings.TrimSpace(createOpts.Image) == "" {
		createOpts.Image = cloud.DefaultImage
	}
	return createOpts, nil
}

func relaunchFreshOffer(cfg RelaunchConfig, group InstanceGroup, minReliability float64, strategy bidding.SelectionStrategy, r2Client *r2.Client) (*cloud.Offer, error) {
	raw := FetchGroupRawOffers(cfg.Clients, []InstanceGroup{group}, minReliability)
	applyDriverFailureExclusions(cfg.Database, r2Client, raw)
	ranked := RankGroupOffersWithPredictor(raw, cfg.PredictorConfig, cfg.SurvivalModel, cfg.SetupFactory, strategy, cfg.MinSurvival)
	if len(ranked) == 0 {
		return nil, ErrNoReplacementOffer
	}
	gOffer := ranked[0]
	if gOffer.Err != nil {
		return nil, gOffer.Err
	}
	if gOffer.Offer == nil {
		constraintStr := FormatOfferConstraints(offerConstraintsForGroup(gOffer.Group, minReliability))
		detail := gOffer.FilterStats.NoOffersDetail(constraintStr)
		if strings.TrimSpace(detail) == "" {
			detail = ErrNoReplacementOffer.Error()
		}
		return nil, fmt.Errorf("%w: %s", ErrNoReplacementOffer, detail)
	}
	return gOffer.Offer, nil
}

func relaunchGroupingJobs(jobs []*db.Job) []*db.Job {
	if len(jobs) == 0 {
		return nil
	}
	grouping := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if job.EffectiveStatus() == db.StatusPendingPlacement && job.TargetKind() == db.JobTargetUnplaced {
			copyJob := *job
			copyJob.Status = db.StatusQueued
			copyJob.PendingStatus = nil
			grouping = append(grouping, &copyJob)
			continue
		}
		grouping = append(grouping, job)
	}
	return grouping
}

// recordJobSkipReason records a per-job skip reason into both NotReplacedReasons
// (keyed by the failed predecessor instance, when known) and JobReasons (keyed
// by job id). Keying in JobReasons ensures the reason survives even when the
// job's last attempt had its launch_id cleared (e.g. after ResetJobToUnplaced),
// where the failedInstanceID lookup in autopilot would otherwise return 0.
func recordJobSkipReason(result *RelaunchResult, jobID, failedInstanceID int64, reason string) {
	if result == nil || reason == "" {
		return
	}
	if result.JobReasons == nil {
		result.JobReasons = map[int64]string{}
	}
	if jobID != 0 {
		result.JobReasons[jobID] = reason
	}
	recordNotReplacedReason(result, failedInstanceID, reason)
}

func recordNotReplacedReason(result *RelaunchResult, failedInstanceID int64, reason string) {
	if result == nil || failedInstanceID == 0 || reason == "" {
		return
	}
	existing, ok := result.NotReplacedReasons[failedInstanceID]
	if !ok {
		result.NotReplacedReasons[failedInstanceID] = reason
		return
	}
	if existing == reason {
		return
	}
	inner := existing
	if strings.HasPrefix(inner, "multiple reasons (") && strings.HasSuffix(inner, ")") {
		inner = strings.TrimSuffix(strings.TrimPrefix(inner, "multiple reasons ("), ")")
	}
	for _, part := range strings.Split(inner, "; ") {
		if part == reason {
			return
		}
	}
	result.NotReplacedReasons[failedInstanceID] = "multiple reasons (" + inner + "; " + reason + ")"
}

// groupSourceUploadError returns the first non-nil source-upload error
// that applies to a group's source directories, or nil if all uploads
// succeeded. The returned error wraps the local directory so the
// downstream skip reason carries enough context to be actionable.
func groupSourceUploadError(group InstanceGroup, perDirErr map[string]error) error {
	if len(perDirErr) == 0 {
		return nil
	}
	for _, dir := range group.SourceDirs() {
		if uploadErr, ok := perDirErr[dir]; ok && uploadErr != nil {
			return fmt.Errorf("upload source %s: %w", dir, uploadErr)
		}
	}
	return nil
}

// recordGroupReasons records reason for every job in the group: both in
// JobReasons (keyed by job ID, covers fresh unplaced jobs) and in
// NotReplacedReasons (keyed by the failed predecessor's instance ID, when
// resetJobs maps the job back to one).
func recordGroupReasons(result *RelaunchResult, resetJobs map[int64]int64, group InstanceGroup, reason string) {
	if result == nil || reason == "" {
		return
	}
	if result.JobReasons == nil {
		result.JobReasons = map[int64]string{}
	}
	seenInstance := map[int64]bool{}
	for _, j := range group.Jobs {
		if j == nil {
			continue
		}
		result.JobReasons[j.ID] = reason
		if failedID := resetJobs[j.ID]; failedID != 0 && !seenInstance[failedID] {
			seenInstance[failedID] = true
			recordNotReplacedReason(result, failedID, reason)
		}
	}
}

func failedInstanceForJob(jobID int64, lastLaunch *db.Launch, resetJobs map[int64]int64) int64 {
	if len(resetJobs) > 0 {
		if id := resetJobs[jobID]; id != 0 {
			return id
		}
	}
	if lastLaunch != nil {
		return lastLaunch.ID
	}
	return 0
}

func budgetMultiplierForFailedInstance(cfg RelaunchConfig, failedInstanceID int64) float64 {
	if failedInstanceID == 0 {
		return 1
	}
	if len(cfg.RetryBudgetMultiplierByFailedInstance) == 0 {
		return 1
	}
	m := cfg.RetryBudgetMultiplierByFailedInstance[failedInstanceID]
	if m <= 0 {
		return 1
	}
	return m
}

func applyRetryBudgetMultiplier(budget RetryBudget, attemptCount int, multiplier float64) RetryBudget {
	if multiplier <= 1 {
		return budget
	}
	scaled := budget
	if attemptCount == 1 {
		if scaled.FirstTimeLimit > 0 {
			scaled.FirstTimeLimit = time.Duration(float64(scaled.FirstTimeLimit) * multiplier)
		}
		if scaled.FirstCostCents > 0 {
			scaled.FirstCostCents = int(math.Round(float64(scaled.FirstCostCents) * multiplier))
		}
		return scaled
	}
	if scaled.NextTimeLimit > 0 {
		scaled.NextTimeLimit = time.Duration(float64(scaled.NextTimeLimit) * multiplier)
	}
	if scaled.NextCostCents > 0 {
		scaled.NextCostCents = int(math.Round(float64(scaled.NextCostCents) * multiplier))
	}
	return scaled
}

func summarizeBudgetDetail(detail string) string {
	if detail == "" {
		return "retry budget exceeded"
	}
	if strings.HasPrefix(detail, "first retry budget exceeded") || strings.HasPrefix(detail, "subsequent retry budget exceeded") {
		return detail
	}
	return "retry budget exceeded: " + detail
}

const runawayPausedReason = "paused: repeated launch failures without progress"
const defaultResumeGracePeriod = 15 * time.Minute

// ResetGlobalRunawayBreaker inserts a global resume event for the runaway
// breaker (campaign_id=NULL, project=<all>). source is recorded in the
// event detail (e.g. "CLI", "TUI") so operators can audit who cleared it.
func ResetGlobalRunawayBreaker(database *sql.DB, source string) error {
	if database == nil {
		return fmt.Errorf("database is nil")
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = "manual"
	}
	return db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchRunawayResumed,
		Detail:    runawayScopeDetail("", "manual reset via "+source),
	})
}

func runawayProjectLabel(project string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		return "<all>"
	}
	return strings.ReplaceAll(project, ";", "_")
}

func runawayScopeDetail(project string, detail string) string {
	label := runawayProjectLabel(project)
	if detail == "" {
		return "project=" + label + ";"
	}
	return "project=" + label + "; " + detail
}

func eventMatchesRunawayProject(event db.LifecycleEvent, project string) bool {
	want := runawayProjectLabel(project)
	got := runawayProjectLabel(runawayProjectFromDetail(event.Detail))
	if want == "<all>" {
		return got == "<all>"
	}
	return got == want || got == "<all>"
}

func runawayProjectFromDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "<all>"
	}
	const key = "project="
	start := strings.Index(detail, key)
	if start < 0 {
		return "<all>"
	}
	value := detail[start+len(key):]
	if end := strings.Index(value, ";"); end >= 0 {
		value = value[:end]
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "<all>"
	}
	return value
}

// latestRunawayEventAt returns the most recent timestamp of a runaway
// lifecycle event matching the given (campaign, project) scope. An event
// counts as matching when its scope is either the same as the requested
// scope or strictly broader: an event with no campaign linkage
// (campaign_id = 0) overrides a campaign-scoped check, and a project of
// "<all>" overrides a project-specific check. This is what makes a global
// reset (e.g. `weft autopilot budget reset`) actually clear a
// campaign-scoped trip.
func latestRunawayEventAt(database *sql.DB, kind string, campaignID int64, project string) int64 {
	events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
		Kind:  kind,
		Limit: 500,
	})
	if err != nil {
		return 0
	}
	for _, e := range events {
		if e.CampaignID != 0 && campaignID != 0 && e.CampaignID != campaignID {
			continue
		}
		if eventMatchesRunawayProject(e, project) {
			return e.OccurredAt
		}
	}
	return 0
}

func evaluateRunawayBreaker(database *sql.DB, cfg RelaunchConfig, unplaced []*db.Job, now time.Time) (bool, string, error) {
	if cfg.RunawayPolicy == nil || !cfg.RunawayPolicy.Enabled || database == nil {
		return false, "", nil
	}
	if len(unplaced) == 0 {
		return false, "", nil
	}
	scopeJobIDs := make([]int64, 0, len(unplaced))
	for _, j := range unplaced {
		if j != nil && !j.HasTag(db.TagInventory) {
			scopeJobIDs = append(scopeJobIDs, j.ID)
		}
	}
	if len(scopeJobIDs) == 0 {
		return false, "", nil
	}
	// campaignID == 0 when launches lack a campaign linkage; the breaker
	// still fires, scoped by job_id alone.
	campaignID := inferScopeCampaignID(database, unplaced)

	resumedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayResumed, campaignID, cfg.ScopeProject)
	trippedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayTripped, campaignID, cfg.ScopeProject)
	if trippedAt > resumedAt {
		reason := runawayPausedReason
		// One blocked event per (trip, scope), not one per pass — the
		// breaker blocks every relaunch pass while tripped, and per-pass
		// rows record no state change. See specs/campaign-lifecycle.allium.
		key := runawayBlockedKey{campaignID: campaignID, project: runawayProjectLabel(cfg.ScopeProject)}
		if prev, loaded := runawayBlockedEventEmitted.Swap(key, trippedAt); !loaded || prev.(int64) != trippedAt {
			_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
				EventKind:  db.EventRelaunchRunawayBlocked,
				CampaignID: campaignID,
				Detail:     runawayScopeDetail(cfg.ScopeProject, reason),
			})
		}
		return true, reason, nil
	}

	// After a manual resume, give new instances time to complete a job
	// before re-evaluating the breaker.
	gracePeriod := cfg.RunawayPolicy.ResumeGracePeriod
	if gracePeriod <= 0 {
		gracePeriod = defaultResumeGracePeriod
	}
	if resumedAt > 0 && now.Before(time.Unix(resumedAt, 0).Add(gracePeriod)) {
		return false, "", nil
	}

	since := now.Add(-cfg.RunawayPolicy.Window).Unix()
	if resumedAt > 0 {
		graceFloor := time.Unix(resumedAt, 0).Add(gracePeriod).Unix()
		if graceFloor > since {
			since = graceFloor
		}
	}
	metrics, err := queryRunawayMetrics(database, campaignID, scopeJobIDs, since, now.Unix())
	if err != nil {
		return false, "", err
	}
	if providers, perr := failingProvidersInWindow(database, scopeJobIDs, since); perr == nil {
		metrics.FailingProviders = providers
	} else {
		slog.Debug("failing providers query failed", "component", "relaunch", "error", perr)
	}
	noProgress := metrics.CompletedCount == 0
	infraLimitHit := cfg.RunawayPolicy.InfraFailureLimit > 0 &&
		metrics.InfraFailureCount >= cfg.RunawayPolicy.InfraFailureLimit
	limitHit := metrics.MaxTrailingOrphaned >= cfg.RunawayPolicy.ChainNoProgressLimit ||
		metrics.OrphanedCount >= cfg.RunawayPolicy.OrphanChurnLimit ||
		metrics.SpendCents >= cfg.RunawayPolicy.SpendNoProgressLimitCent ||
		infraLimitHit
	if !noProgress || !limitHit {
		return false, "", nil
	}
	detail := runawayTripDetail(metrics, cfg.RunawayPolicy)
	_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayTripped,
		CampaignID: campaignID,
		Detail:     runawayScopeDetail(cfg.ScopeProject, detail),
	})
	reason := runawayPausedReason
	if infraLimitHit {
		reason = "paused: repeated infrastructure failures without progress"
	}
	return true, reason, nil
}

func runawayTripDetail(metrics runawayMetrics, policy *RunawayPolicy) string {
	suffix := formatFailingProvidersSuffix(metrics.FailingProviders)
	window := ""
	if policy != nil {
		window = policy.Window.String()
	}
	if policy != nil && policy.InfraFailureLimit > 0 && metrics.InfraFailureCount >= policy.InfraFailureLimit {
		return fmt.Sprintf(
			"infra runaway: infra_failures=%d limit=%d chain=%d orphaned=%d spend=$%.2f window=%s%s",
			metrics.InfraFailureCount,
			policy.InfraFailureLimit,
			metrics.MaxTrailingOrphaned,
			metrics.OrphanedCount,
			float64(metrics.SpendCents)/100.0,
			window,
			suffix,
		)
	}
	return fmt.Sprintf(
		"no-progress runaway: chain=%d orphaned=%d spend=$%.2f window=%s%s",
		metrics.MaxTrailingOrphaned,
		metrics.OrphanedCount,
		float64(metrics.SpendCents)/100.0,
		window,
		suffix,
	)
}

// formatFailingProvidersSuffix produces "; providers=p1:n1,p2:n2"
// (deterministic order) for embedding in a trip detail. Empty when
// no providers contributed failures (degenerate case).
func formatFailingProvidersSuffix(providers map[string]int) string {
	if len(providers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(providers))
	for k := range providers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%d", k, providers[k]))
	}
	return "; providers=" + strings.Join(parts, ",")
}

// ResumeRunawayBreakerForJob clears the runaway breaker for the scope
// associated with the given job (campaign + project). Returns true if a
// breaker was active and has been resumed, false if no breaker was tripped.
func ResumeRunawayBreakerForJob(database *sql.DB, job *db.Job) (bool, error) {
	if database == nil || job == nil {
		return false, nil
	}
	project := job.Project
	campaignID := inferScopeCampaignID(database, []*db.Job{job})
	if campaignID == 0 {
		campaignID = inferActiveRunawayCampaignID(database, project)
	}
	if campaignID == 0 {
		return false, nil
	}
	resumedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayResumed, campaignID, project)
	pausedAt, pausedScope := latestRunawayPausedAt(database, campaignID, project)
	if pausedAt <= resumedAt {
		return false, nil
	}
	resumeProject := project
	if pausedScope == "<all>" {
		resumeProject = ""
	}
	detail := runawayScopeDetail(resumeProject, "auto-resumed via retry")
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayResumed,
		CampaignID: campaignID,
		Detail:     detail,
	}); err != nil {
		return false, fmt.Errorf("record resume event: %w", err)
	}
	return true, nil
}

func inferActiveRunawayCampaignID(database *sql.DB, project string) int64 {
	if database == nil {
		return 0
	}
	events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
		KindPrefix: "relaunch.runaway_",
		Limit:      200,
	})
	if err != nil {
		return 0
	}
	var fallback int64
	for _, event := range events {
		if event.CampaignID == 0 {
			continue
		}
		if event.EventKind != db.EventRelaunchRunawayBlocked && event.EventKind != db.EventRelaunchRunawayTripped {
			continue
		}
		if !eventMatchesRunawayProject(event, project) {
			continue
		}
		resumedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayResumed, event.CampaignID, project)
		pausedAt, _ := latestRunawayPausedAt(database, event.CampaignID, project)
		if pausedAt > resumedAt {
			return event.CampaignID
		}
		if fallback == 0 {
			fallback = event.CampaignID
		}
	}
	return fallback
}

func latestRunawayPausedAt(database *sql.DB, campaignID int64, project string) (int64, string) {
	events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
		CampaignID: campaignID,
		KindPrefix: "relaunch.runaway_",
		Limit:      200,
	})
	if err != nil {
		return 0, ""
	}
	for _, event := range events {
		if event.EventKind != db.EventRelaunchRunawayBlocked && event.EventKind != db.EventRelaunchRunawayTripped {
			continue
		}
		if !eventMatchesRunawayProject(event, project) {
			continue
		}
		return event.OccurredAt, runawayProjectLabel(runawayProjectFromDetail(event.Detail))
	}
	return 0, ""
}

type runawayMetrics struct {
	CompletedCount      int
	OrphanedCount       int
	InfraFailureCount   int
	SpendCents          int
	MaxTrailingOrphaned int
	// FailingProviders maps provider name to the count of failures it
	// contributed in the breaker window. Recorded on trip so the
	// relaunch loop can route around dead providers without blocking
	// launches on healthy ones (a Vast outage shouldn't pause RunPod).
	FailingProviders map[string]int
}

// failingProvidersInWindow returns the per-provider count of distinct launches
// that produced orphaned or failed attempts in the window. Exposed publicly so
// trip-time detail recording and post-trip provider-bypass logic share the same
// query. Counting launches keeps one bad rental carrying many queued jobs from
// looking like many independent provider failures.
func failingProvidersInWindow(database *sql.DB, jobIDs []int64, since int64) (map[string]int, error) {
	if len(jobIDs) == 0 {
		return nil, nil
	}
	holders := make([]string, 0, len(jobIDs))
	args := make([]any, 0, len(jobIDs)+1)
	for _, id := range jobIDs {
		holders = append(holders, "?")
		args = append(args, id)
	}
	args = append(args, since)
	rows, err := database.Query(fmt.Sprintf(`
		SELECT COALESCE(l.provider, '') AS prov, COUNT(DISTINCT ja.launch_id)
		  FROM job_attempts ja
		  JOIN launches l ON l.id = ja.launch_id
		 WHERE ja.launch_id IS NOT NULL
		   AND ja.cloud_outcome IN ('orphaned', 'failed')
		   AND ja.job_id IN (%s)
		   AND COALESCE(ja.end_time, ja.start_time, 0) >= ?
		 GROUP BY prov`, strings.Join(holders, ",")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var prov string
		var n int
		if err := rows.Scan(&prov, &n); err != nil {
			return nil, err
		}
		if strings.TrimSpace(prov) == "" {
			continue
		}
		out[prov] = n
	}
	return out, rows.Err()
}

// providersFromTripDetail extracts the per-provider failure counts
// embedded in a runaway-tripped event detail.
// Format: "...; providers=vastai:7,runpod:0".
func providersFromTripDetail(detail string) map[string]int {
	out := map[string]int{}
	const marker = "providers="
	idx := strings.Index(detail, marker)
	if idx < 0 {
		return out
	}
	chunk := detail[idx+len(marker):]
	if end := strings.IndexByte(chunk, ';'); end >= 0 {
		chunk = chunk[:end]
	}
	for _, pair := range strings.Split(chunk, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			continue
		}
		out[strings.TrimSpace(parts[0])] = n
	}
	return out
}

// unplacedForHealthyProviders returns the subset of `jobs` whose
// pinned provider has zero failures on the most recent runaway trip.
// Jobs without a provider tag stay blocked: they could land on the
// failing provider. Older trips lacking provider stats block all
// providers (conservative).
func unplacedForHealthyProviders(database *sql.DB, jobs []*db.Job) []*db.Job {
	if database == nil || len(jobs) == 0 {
		return nil
	}
	trippedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayTripped, 0, "")
	resumedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayResumed, 0, "")
	if trippedAt <= resumedAt {
		return jobs
	}
	tripped, err := db.LatestLifecycleEvent(database, db.LifecycleEventFilter{
		Kind: db.EventRelaunchRunawayTripped,
	})
	if err != nil || tripped == nil {
		return nil
	}
	providers := providersFromTripDetail(tripped.Detail)
	if len(providers) == 0 {
		return nil
	}
	out := make([]*db.Job, 0, len(jobs))
	for _, j := range jobs {
		if j == nil {
			continue
		}
		provider, ok := db.RequestedProvider(j.Tags)
		if !ok {
			continue
		}
		if providers[provider] > 0 {
			continue
		}
		out = append(out, j)
	}
	return out
}

func inferScopeCampaignID(database *sql.DB, jobs []*db.Job) int64 {
	for _, j := range jobs {
		if j == nil {
			continue
		}
		facts, err := attemptFactsForRelaunch(database, j.ID)
		if err != nil || facts.LastLaunch == nil || facts.LastLaunch.CampaignID == nil {
			continue
		}
		return *facts.LastLaunch.CampaignID
	}
	return 0
}

func campaignFilter(alias string, campaignID int64) (string, []any) {
	if campaignID == 0 {
		return "", nil
	}
	return fmt.Sprintf(" AND %s.campaign_id = ?", alias), []any{campaignID}
}

func queryRunawayMetrics(database *sql.DB, campaignID int64, jobIDs []int64, since int64, nowUnix int64) (runawayMetrics, error) {
	if len(jobIDs) == 0 {
		return runawayMetrics{}, nil
	}
	var m runawayMetrics
	args := make([]any, 0, len(jobIDs)+4)
	holders := make([]string, 0, len(jobIDs))
	for _, id := range jobIDs {
		holders = append(holders, "?")
		args = append(args, id)
	}
	inClause := strings.Join(holders, ",")

	infraReasons := db.InfrastructureTerminationReasons()
	infraPlaceholders := strings.Repeat("?,", len(infraReasons))
	infraPlaceholders = strings.TrimRight(infraPlaceholders, ",")
	infraArgs := make([]any, 0, len(infraReasons))
	for _, r := range infraReasons {
		infraArgs = append(infraArgs, r)
	}

	countClause, countCampaignArgs := campaignFilter("l", campaignID)
	// Completed counts every cloud_outcome=completed attempt; orphaned counts
	// only orphans whose launch wasn't an infrastructure-side failure
	// (provider/infra/bootstrap-timeout/phase-stall/preempted). Infra failures
	// count distinct failed launches rather than job attempts, so one bad rental
	// carrying many queued jobs contributes one infrastructure-weather sample.
	countArgs := append([]any{db.AttemptOutcomeCompleted, db.AttemptOutcomeOrphaned}, infraArgs...)
	countArgs = append(countArgs, db.AttemptOutcomeOrphaned)
	countArgs = append(countArgs, infraArgs...)
	countArgs = append(countArgs, args...)
	countArgs = append(countArgs, countCampaignArgs...)
	countArgs = append(countArgs, since)
	row := database.QueryRow(
		fmt.Sprintf(`SELECT
			COALESCE(SUM(CASE WHEN ja.cloud_outcome = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE
				WHEN ja.cloud_outcome = ?
				 AND COALESCE(l.termination_reason, '') NOT IN (%s)
				THEN 1 ELSE 0 END), 0),
			COALESCE(COUNT(DISTINCT CASE
				WHEN ja.cloud_outcome = ?
				 AND COALESCE(l.termination_reason, '') IN (%s)
				THEN ja.launch_id END), 0)
		FROM job_attempts ja
		JOIN launches l ON l.id = ja.launch_id
		WHERE ja.launch_id IS NOT NULL
		  AND ja.job_id IN (%s)%s
		  AND COALESCE(ja.end_time, ja.start_time, 0) >= ?`, infraPlaceholders, infraPlaceholders, inClause, countClause),
		countArgs...,
	)
	if err := row.Scan(&m.CompletedCount, &m.OrphanedCount, &m.InfraFailureCount); err != nil {
		return m, err
	}

	spendClause, spendCampaignArgs := campaignFilter("l2", campaignID)
	spendArgs := append([]any{nowUnix}, args...)
	spendArgs = append(spendArgs, spendCampaignArgs...)
	spendArgs = append(spendArgs, since, db.LaunchStatusFailed, db.LaunchStatusCancelled)
	spendRow := database.QueryRow(
		fmt.Sprintf(`SELECT COALESCE(SUM(
			CASE
				WHEN l.actual_spend_cents > 0 THEN l.actual_spend_cents
				WHEN l.cost_per_hour_cents > 0 THEN CAST(ROUND(
					(MAX(0, COALESCE(l.ended_at, ?) - COALESCE(l.launched_at, l.created_at))) / 3600.0
					* l.cost_per_hour_cents
				) AS INTEGER)
				ELSE 0
			END
		), 0)
		FROM launches l
		JOIN (
			SELECT DISTINCT ja.launch_id
			FROM job_attempts ja
			JOIN launches l2 ON l2.id = ja.launch_id
			WHERE ja.launch_id IS NOT NULL
			  AND ja.job_id IN (%s)%s
			  AND COALESCE(ja.end_time, ja.start_time, 0) >= ?
		) scoped ON scoped.launch_id = l.id
		WHERE l.status IN (?, ?)`, inClause, spendClause),
		spendArgs...,
	)
	if err := spendRow.Scan(&m.SpendCents); err != nil {
		return m, err
	}

	launchCache := map[int64]*db.Launch{}
	for _, jobID := range jobIDs {
		attempts, err := db.GetLaunchAttempts(database, jobID)
		if err != nil || len(attempts) == 0 {
			continue
		}
		trailing := 0
		for i := len(attempts) - 1; i >= 0; i-- {
			a := attempts[i]
			if a.StartedAt < since {
				break
			}
			ci, ok := launchCache[a.LaunchID]
			if !ok {
				ci, _ = db.GetLaunch(database, a.LaunchID)
				launchCache[a.LaunchID] = ci
			}
			if ci == nil {
				continue
			}
			if campaignID != 0 && (ci.CampaignID == nil || *ci.CampaignID != campaignID) {
				continue
			}
			if a.Outcome == db.AttemptOutcomeOrphaned {
				// Infra-side failures are neutral: they don't extend the
				// chain (Vast flakiness shouldn't trip the breaker) but
				// also don't break it (an infra failure between two real
				// orphans still indicates a stuck job).
				if db.IsInfrastructureTermination(ci.TerminationReason) {
					continue
				}
				trailing++
				continue
			}
			break
		}
		if trailing > m.MaxTrailingOrphaned {
			m.MaxTrailingOrphaned = trailing
		}
	}
	return m, nil
}

// mostRecentDiskFullGB returns the disk_gb of the most recent cloud instance
// that failed with disk_full for any job in the group. Returns 0 if no such
// instance exists.
func mostRecentDiskFullGB(database *sql.DB, group InstanceGroup) int {
	for _, j := range group.Jobs {
		attempts, err := db.GetLaunchAttempts(database, j.ID)
		if err != nil || len(attempts) == 0 {
			continue
		}
		last := attempts[len(attempts)-1]
		ci, err := db.GetLaunch(database, last.LaunchID)
		if err != nil || ci == nil {
			continue
		}
		if ci.TerminationReason == db.TerminationReasonDiskFull && ci.DiskGB > 0 {
			return ci.DiskGB
		}
	}
	return 0
}

type driverFailureProbeReport struct {
	RequiredMajor int    `json:"required_driver_major"`
	ActualMajor   int    `json:"actual_driver_major"`
	ActualVersion string `json:"actual_driver_version"`
}

type driverFailureExclusions struct {
	machineKeys    map[string]struct{}
	gpuDataCenters map[string]struct{}
	gpuNames       map[string]struct{}
}

type driverFailureExclusionSummary struct {
	Removed int
	Reasons []string
}

func applyDriverFailureExclusions(database *sql.DB, r2Client *r2.Client, raw []GroupRawOffers) []driverFailureExclusionSummary {
	summaries := make([]driverFailureExclusionSummary, len(raw))
	if database == nil || r2Client == nil || len(raw) == 0 {
		return summaries
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := range raw {
		if raw[i].Err != nil || len(raw[i].Offers) == 0 {
			continue
		}
		exclusions := driverFailureExclusionsForGroup(ctx, database, r2Client, raw[i].Group)
		filtered, summary := filterOffersByDriverFailureExclusions(raw[i].Offers, exclusions)
		if summary.Removed == 0 {
			continue
		}
		slog.Info("filtered offers by prior driver failure",
			"component", "relaunch",
			"gpu_spec", raw[i].Group.GPUSpec(),
			"filtered", summary.Removed,
			"remaining", len(filtered),
			"reason", strings.Join(summary.Reasons, "; "))
		raw[i].Offers = filtered
		summaries[i] = summary
	}
	return summaries
}

func driverFailureExclusionsForGroup(ctx context.Context, database *sql.DB, r2Client *r2.Client, group InstanceGroup) driverFailureExclusions {
	out := driverFailureExclusions{
		machineKeys:    map[string]struct{}{},
		gpuDataCenters: map[string]struct{}{},
		gpuNames:       map[string]struct{}{},
	}
	seenLaunches := map[int64]struct{}{}
	for _, j := range group.Jobs {
		attempts, err := db.GetLaunchAttempts(database, j.ID)
		if err != nil {
			continue
		}
		for i := len(attempts) - 1; i >= 0; i-- {
			launchID := attempts[i].LaunchID
			if _, seen := seenLaunches[launchID]; seen {
				continue
			}
			seenLaunches[launchID] = struct{}{}
			launch, err := db.GetLaunch(database, launchID)
			if err != nil || launch == nil {
				continue
			}
			report, ok := readDriverFailureProbeReport(ctx, r2Client, launchID)
			if !ok || !driverFailureReportApplies(report, group) {
				continue
			}
			gpuName := launchDriverFailureGPUName(launch)
			if gpuName == "" {
				continue
			}
			// RunPod search offers are GPU-type scoped; machine/datacenter
			// avoidance for RunPod needs provider-specific handling.
			if launch.Provider != string(cloud.ProviderRunpod) {
				if key := db.ProviderMachineKey(launch.Provider, launch.MachineID); key != "" {
					out.machineKeys[key] = struct{}{}
				}
				if launch.DataCenter != "" {
					out.gpuDataCenters[driverFailureGPUDataCenterKey(launch.Provider, gpuName, launch.DataCenter)] = struct{}{}
				}
			}
			if !driverFailureGPUNameSatisfiesGroup(group, gpuName) {
				out.gpuNames[driverFailureGPUKey(launch.Provider, gpuName)] = struct{}{}
			}
		}
	}
	return out
}

func readDriverFailureProbeReport(ctx context.Context, r2Client *r2.Client, launchID int64) (driverFailureProbeReport, bool) {
	if r2Client == nil || launchID <= 0 {
		return driverFailureProbeReport{}, false
	}
	data, err := r2Client.GetObject(ctx, r2keys.InstanceDriverFailure(launchID))
	if err != nil {
		return driverFailureProbeReport{}, false
	}
	var report driverFailureProbeReport
	if err := json.Unmarshal(data, &report); err != nil {
		return driverFailureProbeReport{}, false
	}
	return report, true
}

func driverFailureReportApplies(report driverFailureProbeReport, group InstanceGroup) bool {
	if report.ActualMajor <= 0 {
		return false
	}
	required := report.RequiredMajor
	if group.MinDriverVersion > required {
		required = group.MinDriverVersion
	}
	return required > 0 && report.ActualMajor < required
}

func launchDriverFailureGPUName(launch *db.Launch) string {
	if launch == nil {
		return ""
	}
	for _, candidate := range []string{launch.ResolvedGPUName, launch.GPUClass, launch.GPUSpec} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	return ""
}

func filterOffersByDriverFailureExclusions(offers []cloud.Offer, exclusions driverFailureExclusions) ([]cloud.Offer, driverFailureExclusionSummary) {
	if len(exclusions.machineKeys) == 0 && len(exclusions.gpuDataCenters) == 0 && len(exclusions.gpuNames) == 0 {
		return offers, driverFailureExclusionSummary{}
	}
	out := offers[:0:0]
	summary := driverFailureExclusionSummary{}
	reasons := map[string]struct{}{}
	for _, offer := range offers {
		reason := driverFailureOfferExclusionReason(offer, exclusions)
		if reason == "" {
			out = append(out, offer)
			continue
		}
		summary.Removed++
		reasons[reason] = struct{}{}
	}
	if summary.Removed > 0 {
		summary.Reasons = sortedDriverFailureReasons(reasons)
	}
	return out, summary
}

func driverFailureOfferExclusionReason(offer cloud.Offer, exclusions driverFailureExclusions) string {
	if key := offerMachineClaimKey(offer); key != "" {
		if _, blocked := exclusions.machineKeys[key]; blocked {
			return "same provider machine"
		}
	}
	if key := driverFailureGPUDataCenterKey(string(offer.Provider), offer.GPUName, offer.DataCenter); key != "" {
		if _, blocked := exclusions.gpuDataCenters[key]; blocked {
			return "same gpu/datacenter"
		}
	}
	if key := driverFailureGPUKey(string(offer.Provider), offer.GPUName); key != "" {
		if _, blocked := exclusions.gpuNames[key]; blocked {
			return "prior incompatible gpu"
		}
	}
	return ""
}

func driverFailureGPUNameSatisfiesGroup(group InstanceGroup, gpuName string) bool {
	gpuName = strings.TrimSpace(gpuName)
	if gpuName == "" || strings.TrimSpace(group.GPUClass) == "" {
		return true
	}
	constraint := placement.ParseGPUConstraint(group.GPUClass)
	return constraint.MatchesGPUFullName(gpuName) || constraint.MatchesGPU(driverFailureGPUClassKey(gpuName))
}

func driverFailureGPUDataCenterKey(provider, gpuName, dataCenter string) string {
	provider = strings.TrimSpace(provider)
	gpuKey := driverFailureGPUClassKey(gpuName)
	dataCenter = strings.ToLower(strings.TrimSpace(dataCenter))
	if provider == "" || gpuKey == "" || dataCenter == "" {
		return ""
	}
	return provider + "/" + gpuKey + "/" + dataCenter
}

func driverFailureGPUKey(provider, gpuName string) string {
	provider = strings.TrimSpace(provider)
	gpuKey := driverFailureGPUClassKey(gpuName)
	if provider == "" || gpuKey == "" {
		return ""
	}
	return provider + "/" + gpuKey
}

func driverFailureGPUClassKey(gpuName string) string {
	gpuName = strings.ToLower(strings.TrimSpace(gpuName))
	var b strings.Builder
	for _, r := range gpuName {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	key := b.String()
	key = strings.TrimPrefix(key, "nvidiageforce")
	key = strings.TrimPrefix(key, "nvidia")
	return key
}

func sortedDriverFailureReasons(reasons map[string]struct{}) []string {
	out := make([]string, 0, len(reasons))
	for reason := range reasons {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}

func appendDriverFailureExclusionDetail(detail string, summary driverFailureExclusionSummary) string {
	if summary.Removed == 0 {
		return detail
	}
	suffix := fmt.Sprintf("driver-failure retry exclusions removed %d offers", summary.Removed)
	if len(summary.Reasons) > 0 {
		suffix += " (" + strings.Join(summary.Reasons, ", ") + ")"
	}
	if strings.TrimSpace(detail) == "" {
		return suffix
	}
	return detail + "; " + suffix
}

// countAttemptsForRelaunch returns the number of launch attempts for a job,
// scoped to the campaign of the job's most recent attempt. This prevents
// attempts from earlier campaigns from exhausting the retry budget.
type relaunchAttemptFacts struct {
	Count                int
	LastLaunch           *db.Launch
	LastAttemptStartTime int64
	LastAttemptEndTime   *int64
}

// backoffRemaining returns how long the caller must still wait before
// re-launching this job, given facts.Count consecutive prior attempts. Zero
// means the job is eligible (including when there are no prior attempts or
// the last attempt's end time is unknown — see retrypolicy.BackoffRemaining).
func backoffRemaining(facts relaunchAttemptFacts, now time.Time) time.Duration {
	if facts.LastAttemptEndTime == nil {
		return 0
	}
	return retrypolicy.BackoffRemaining(facts.Count, time.Unix(*facts.LastAttemptEndTime, 0), now)
}

func attemptFactsForRelaunch(database *sql.DB, jobID int64) (relaunchAttemptFacts, error) {
	attempts, err := db.GetLaunchAttempts(database, jobID)
	if err != nil || len(attempts) == 0 {
		return relaunchAttemptFacts{}, err
	}
	// Skip canceled attempts: their duration reflects cleanup of a stuck
	// rental, not retry-system activity, and must not charge the budget.
	var lastAttempt db.LaunchAttempt
	haveLast := false
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].Outcome == db.AttemptOutcomeCancelled {
			continue
		}
		lastAttempt = attempts[i]
		haveLast = true
		break
	}
	if !haveLast {
		count, countErr := db.CountLaunchAttempts(database, jobID)
		return relaunchAttemptFacts{Count: count}, countErr
	}

	var (
		startTime sql.NullInt64
		endTime   sql.NullInt64
	)
	_ = database.QueryRow(
		`SELECT start_time, end_time
		   FROM job_attempts
		  WHERE id = ?`,
		lastAttempt.ID,
	).Scan(&startTime, &endTime)

	var lastStart int64
	var lastEnd *int64
	if startTime.Valid && startTime.Int64 > 0 {
		lastStart = startTime.Int64
	}
	if endTime.Valid {
		v := endTime.Int64
		lastEnd = &v
	}

	ci, err := db.GetLaunch(database, lastAttempt.LaunchID)
	if err != nil || ci == nil {
		count, countErr := db.CountLaunchAttempts(database, jobID)
		return relaunchAttemptFacts{
			Count:                count,
			LastLaunch:           nil,
			LastAttemptStartTime: lastStart,
			LastAttemptEndTime:   lastEnd,
		}, countErr
	}
	if ci.CampaignID == nil {
		count, countErr := db.CountLaunchAttempts(database, jobID)
		return relaunchAttemptFacts{
			Count:                count,
			LastLaunch:           ci,
			LastAttemptStartTime: lastStart,
			LastAttemptEndTime:   lastEnd,
		}, countErr
	}
	count, countErr := db.CountLaunchAttemptsInCampaign(database, jobID, *ci.CampaignID)
	return relaunchAttemptFacts{
		Count:                count,
		LastLaunch:           ci,
		LastAttemptStartTime: lastStart,
		LastAttemptEndTime:   lastEnd,
	}, countErr
}

func retryBudgetUsage(facts relaunchAttemptFacts, now time.Time) (time.Duration, int) {
	// If this attempt never started, it should not consume retry runtime/spend budget.
	if facts.LastAttemptStartTime <= 0 {
		return 0, 0
	}
	start := time.Unix(facts.LastAttemptStartTime, 0)
	end := now
	if facts.LastAttemptEndTime != nil && *facts.LastAttemptEndTime > 0 {
		end = time.Unix(*facts.LastAttemptEndTime, 0)
	}
	if end.Before(start) {
		end = start
	}
	elapsed := end.Sub(start)
	if elapsed <= 0 {
		return 0, 0
	}

	ci := facts.LastLaunch
	if ci == nil {
		return elapsed, 0
	}

	launchElapsed, launchSpend := launchElapsedAndSpendCents(ci, now)
	if ci.ActualSpendCents > 0 && launchElapsed > 0 {
		ratio := float64(elapsed) / float64(launchElapsed)
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		spend := int(math.Round(float64(ci.ActualSpendCents) * ratio))
		return elapsed, max(0, spend)
	}
	if ci.CostPerHourCents > 0 {
		spend := int(math.Round(elapsed.Hours() * float64(ci.CostPerHourCents)))
		return elapsed, max(0, spend)
	}
	return elapsed, max(0, launchSpend)
}

func launchElapsedAndSpendCents(ci *db.Launch, now time.Time) (time.Duration, int) {
	if ci == nil {
		return 0, 0
	}
	startUnix := ci.CreatedAt
	if ci.LaunchedAt != nil && *ci.LaunchedAt > 0 {
		startUnix = *ci.LaunchedAt
	}
	if startUnix <= 0 {
		return 0, 0
	}
	start := time.Unix(startUnix, 0)
	end := now
	if ci.EndedAt != nil && *ci.EndedAt > 0 {
		end = time.Unix(*ci.EndedAt, 0)
	}
	if end.Before(start) {
		end = start
	}
	elapsed := end.Sub(start)

	if ci.ActualSpendCents > 0 {
		return elapsed, ci.ActualSpendCents
	}
	if ci.CostPerHourCents <= 0 || elapsed <= 0 {
		return elapsed, 0
	}
	cents := int(math.Round(elapsed.Hours() * float64(ci.CostPerHourCents)))
	if cents < 0 {
		cents = 0
	}
	return elapsed, cents
}

func exceedsRetryBudget(budget RetryBudget, attemptCount int, elapsed time.Duration, spendCents int) (bool, string) {
	limitTime := budget.NextTimeLimit
	limitCost := budget.NextCostCents
	tier := "subsequent"
	if attemptCount == 1 {
		limitTime = budget.FirstTimeLimit
		limitCost = budget.FirstCostCents
		tier = "first"
	}
	if limitTime > 0 && elapsed >= limitTime {
		return true, fmt.Sprintf("%s retry budget exceeded: elapsed %s >= limit %s",
			tier, elapsed.Truncate(time.Second), limitTime.Truncate(time.Second))
	}
	if limitCost > 0 && spendCents >= limitCost {
		return true, fmt.Sprintf("%s retry budget exceeded: spend $%.2f >= limit $%.2f",
			tier, float64(spendCents)/100.0, float64(limitCost)/100.0)
	}
	return false, ""
}

// openRelaunchIntents creates an open auto_relaunch placement intent for each
// job in the group. Jobs that already have an open intent (from another path)
// are silently skipped — those are being placed elsewhere and we should not
// overwrite that intent's resolution. Returns the IDs of the intents this
// call created so closeRelaunchIntents can resolve them.
func openRelaunchIntents(database *sql.DB, jobs []*db.Job) []int64 {
	if database == nil || len(jobs) == 0 {
		return nil
	}
	out := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		if j == nil || j.ID <= 0 {
			continue
		}
		intent, err := db.CreatePlacementIntent(database, j.ID, "auto_relaunch")
		if err != nil {
			if errors.Is(err, db.ErrPlacementIntentAlreadyOpen) {
				continue
			}
			slog.Warn("open auto_relaunch placement intent",
				"component", "relaunch", "job_id", j.ID, "error", err)
			continue
		}
		out = append(out, intent.ID)
	}
	return out
}

// ReuseRetryBlocked reports whether a job's launch-attempt history blocks
// an automatic reuse placement: the per-job attempt cap or the retry
// backoff window. Shared by the relaunch reuse pass and the autopilot
// reuse-assignment path so a job that instances repeatedly bounce (submit
// succeeds, attempt orphans, job returns to unplaced) cannot cycle through
// either path without bound. An attempt-lookup error blocks — placement
// decisions fail closed on unknown. maxAttempts <= 0 uses
// DefaultMaxCloudAttempts. The reason is a short human-readable cause for
// diagnostics; callers that record their own reasons may ignore it.
func ReuseRetryBlocked(database *sql.DB, jobID int64, maxAttempts int, now time.Time) (bool, string) {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxCloudAttempts
	}
	facts, err := attemptFactsForRelaunch(database, jobID)
	if err != nil {
		return true, "attempt history unavailable: " + err.Error()
	}
	if facts.Count >= maxAttempts {
		return true, fmt.Sprintf("max cloud attempts reached (%d)", facts.Count)
	}
	if remaining := backoffRemaining(facts, now); remaining > 0 {
		return true, fmt.Sprintf("retry backoff %s remaining (after %d failure(s))", remaining.Truncate(time.Second), facts.Count)
	}
	return false, ""
}

// tryPlaceOntoExistingInstances attempts to place each unplaced cloud job
// onto an already-running cloud instance via ReuseSource scoring. Returns
// the subset of jobs that could not be placed (those still need a fresh
// instance). Runs ahead of the runaway-breaker check so that consumers of
// running producers ride the producer's instance even when the breaker
// has paused new launches. The breaker is the only limiter this pass is
// exempt from: the per-job attempt cap and backoff apply here just as in
// the new-instance path, so a job that instances repeatedly bounce
// (submit succeeds, attempt orphans, job returns to unplaced) cannot
// re-submit every pass without bound — a placement succeeding is not
// evidence the job will stick.
func tryPlaceOntoExistingInstances(cfg RelaunchConfig, jobs []*db.Job, maxAttempts int) []*db.Job {
	if cfg.Database == nil || len(jobs) == 0 {
		return jobs
	}
	if cfg.R2Cfg.Bucket == "" || cfg.R2Cfg.AccessKeyID == "" {
		return jobs
	}
	r2Client, err := r2.New(r2.Config{
		AccountID:       cfg.R2Cfg.AccountID,
		AccessKeyID:     cfg.R2Cfg.AccessKeyID,
		SecretAccessKey: cfg.R2Cfg.SecretAccessKey,
		Bucket:          cfg.R2Cfg.Bucket,
	})
	if err != nil {
		slog.Debug("reuse pass: r2 client unavailable", "component", "relaunch", "error", err)
		return jobs
	}

	sources := []placement.CandidateSource{&ReuseSource{}}
	remaining := make([]*db.Job, 0, len(jobs))
	ctx := context.Background()
	for _, j := range jobs {
		if j == nil || j.HasTag(db.TagInventory) {
			remaining = append(remaining, j)
			continue
		}
		if j.LaunchID != nil && *j.LaunchID != 0 {
			continue // already placed
		}
		// Attempt cap and backoff: skip to `remaining`, where the
		// new-instance eligibility loop re-evaluates the same limits and
		// records the cap/backoff skip reason.
		if blocked, _ := ReuseRetryBlocked(cfg.Database, j.ID, maxAttempts, time.Now()); blocked {
			remaining = append(remaining, j)
			continue
		}
		constraints := placement.ConstraintsFromJob(j)
		// Bias scoring toward the live instance hosting any --needs producer
		// so consumers co-locate with their producers (zero-wait estimate
		// in reuse_source.go). cmd/run.go does this at submit time; the
		// daemon-side relaunch path needs the same hint.
		constraints.PreferredInstanceIDs = PreferredInstanceIDsFromNeeds(cfg.Database, j.Needs)
		// Skip dep-blocked consumers only when no live producer instance can
		// accept them. Same-instance --needs edges are valid: the agent gates
		// the consumer behind the producer via cloud_after.
		if _, blocked := queueblock.WaitingOnProducerReason(cfg.Database, j); blocked && len(constraints.PreferredInstanceIDs) == 0 {
			remaining = append(remaining, j)
			continue
		}

		plan, err := placement.Evaluate(placement.EvaluateRequest{
			Constraints: constraints,
			Sources:     sources,
			Database:    cfg.Database,
		})
		if err != nil || plan == nil || plan.Unplaced {
			remaining = append(remaining, j)
			continue
		}
		pick := plan.Fast
		if pick == nil {
			pick = plan.Cheap
		}
		if pick == nil || pick.Kind != placement.CandidateCloudReuse || pick.Reuse == nil {
			remaining = append(remaining, j)
			continue
		}
		instanceID := pick.Reuse.InstanceID
		if err := SubmitJobsToInstance(ctx, cfg.Database, r2Client, instanceID, []*db.Job{j}); err != nil {
			slog.Warn("reuse pass submit failed",
				"component", "relaunch", "job_id", j.ID, "instance", instanceID, "error", err)
			remaining = append(remaining, j)
			continue
		}
		slog.Info("reuse pass placed job onto existing instance",
			"component", "relaunch", "job_id", j.ID, "instance", instanceID)
	}
	return remaining
}

// PreferredInstanceIDsFromNeeds returns the set of live rental instance IDs
// hosting any --needs producer of the consumer job, used as a soft placement
// tip so consumers co-locate with their producers
// (see internal/placement.Constraints.PreferredInstanceIDs). Best-effort:
// skips malformed specs, missing producers, and on-prem producers.
func PreferredInstanceIDsFromNeeds(database *sql.DB, needs []string) []int64 {
	if database == nil || len(needs) == 0 {
		return nil
	}
	seen := make(map[int64]bool)
	var out []int64
	for _, spec := range needs {
		parsed, err := runner.ParseNeedsSpec(spec)
		if err != nil {
			continue
		}
		if parsed.IsAsset() {
			// Named assets don't bias placement toward any instance; they're
			// staged from R2 wherever the consumer lands.
			continue
		}
		producer, err := db.GetJobByID(database, parsed.Version)
		if err != nil || producer == nil {
			continue
		}
		if producer.LaunchID == nil || *producer.LaunchID == 0 {
			continue
		}
		if seen[*producer.LaunchID] {
			continue
		}
		seen[*producer.LaunchID] = true
		out = append(out, *producer.LaunchID)
	}
	return out
}

// closeRelaunchIntents resolves the intents opened by openRelaunchIntents.
// success=true → confirmed; success=false → canceled. resolution is the
// human-readable explanation persisted on the intent row.
func closeRelaunchIntents(database *sql.DB, intentIDs []int64, success bool, resolution string) {
	if database == nil || len(intentIDs) == 0 {
		return
	}
	state := db.PlacementIntentStateConfirmed
	if !success {
		state = db.PlacementIntentStateCanceled
	}
	for _, id := range intentIDs {
		if err := db.ResolvePlacementIntent(database, id, state, resolution); err != nil {
			slog.Warn("resolve auto_relaunch placement intent",
				"component", "relaunch", "intent_id", id, "error", err)
		}
	}
}

// DefaultAutoProbeInterval is how long we wait between probe launches
// while the runaway breaker is tripped. ~1h matches the bimodal failure
// pattern (good-day vs bad-day) without burning more than ~$1.20/day in
// probe cost during sustained outages (assuming ~$0.05 per probe).
const DefaultAutoProbeInterval = 1 * time.Hour

const DefaultAutoProbeIntervalAfterRecentSuccess = 15 * time.Minute
const DefaultAutoProbeRecentSuccessWindow = 6 * time.Hour

// MaybeAutoResumeBreaker resets the runaway breaker when the most recent
// auto-probe launch produced evidence of provider health: either the
// launch completed cleanly, or any of its job_attempts reached
// status='completed'. The job-completion fallback matters because the
// agent can go silent during finalization, ending the launch in `failed`
// even after a job ran to exit 0.
func MaybeAutoResumeBreaker(database *sql.DB) error {
	if database == nil {
		return nil
	}
	probeLaunchID, err := latestAutoProbeLaunchID(database)
	if err != nil || probeLaunchID == 0 {
		return err
	}
	launch, err := db.GetLaunch(database, probeLaunchID)
	if err != nil || launch == nil {
		return err
	}
	healthy := launch.Status == db.LaunchStatusCompleted
	if !healthy {
		var completedJobs int
		if err := database.QueryRow(
			`SELECT COUNT(*) FROM job_attempts WHERE launch_id = ? AND status = 'completed'`,
			probeLaunchID,
		).Scan(&completedJobs); err != nil {
			return err
		}
		healthy = completedJobs > 0
	}
	if !healthy {
		return nil
	}
	// Only reset if breaker is actually tripped (avoid spurious resume
	// events when the breaker was already cleared by a manual reset).
	trippedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayTripped, 0, "")
	resumedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayResumed, 0, "")
	if trippedAt <= resumedAt {
		return nil
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchAutoProbeResumed,
		LaunchID:  probeLaunchID,
		Detail:    runawayScopeDetail("", fmt.Sprintf("auto-resume after probe launch %d delivered job throughput", probeLaunchID)),
	}); err != nil {
		slog.Warn("write auto-probe-resumed event", "component", "relaunch", "error", err)
	}
	return ResetGlobalRunawayBreaker(database, "auto-probe")
}

// MaybeLaunchAutoProbe launches a single probe instance when:
//   - the runaway breaker is currently tripped,
//   - auto-probe is enabled in the policy (interval > 0),
//   - the last probe (or trip, if no probe yet) was longer ago than the
//     interval, and
//   - there is at least one eligible unplaced job in scope.
//
// The probe takes the cheapest eligible job and routes it through the
// normal relaunch path with BypassRunawayBreaker=true. On success the
// reconciler will mark the launch completed and the next call to
// MaybeAutoResumeBreaker will lift the breaker.
//
// Returns the probe launch ID (0 if no probe was launched) and any error.
func MaybeLaunchAutoProbe(cfg RelaunchConfig) (int64, error) {
	if cfg.RunawayPolicy == nil || cfg.Database == nil {
		return 0, nil
	}
	interval := cfg.RunawayPolicy.AutoProbeInterval
	if interval <= 0 {
		return 0, nil
	}
	now := time.Now()

	trippedAt := latestRunawayEventAt(cfg.Database, db.EventRelaunchRunawayTripped, 0, "")
	resumedAt := latestRunawayEventAt(cfg.Database, db.EventRelaunchRunawayResumed, 0, "")
	if trippedAt <= resumedAt {
		// Breaker is not tripped; nothing to probe.
		return 0, nil
	}

	// Recent successful auto-resume → flaky provider, not cold outage:
	// shorten cadence so we don't miss the next working window.
	if cfg.RunawayPolicy.AutoProbeIntervalAfterRecentSuccess > 0 &&
		cfg.RunawayPolicy.AutoProbeRecentSuccessWindow > 0 {
		lastResumed := latestRunawayEventAt(cfg.Database, db.EventRelaunchAutoProbeResumed, 0, "")
		if lastResumed > 0 && now.Sub(time.Unix(lastResumed, 0)) <= cfg.RunawayPolicy.AutoProbeRecentSuccessWindow {
			interval = cfg.RunawayPolicy.AutoProbeIntervalAfterRecentSuccess
		}
	}

	lastProbe := latestRunawayEventAt(cfg.Database, db.EventRelaunchAutoProbeLaunched, 0, "")
	gateAt := trippedAt
	if lastProbe > gateAt {
		gateAt = lastProbe
	}
	if now.Sub(time.Unix(gateAt, 0)) < interval {
		return 0, nil
	}

	probeJob, err := pickAutoProbeJob(cfg)
	if err != nil || probeJob == nil {
		return 0, err
	}

	probeCfg := cfg
	probeCfg.ScopeJobIDs = []int64{probeJob.ID}
	probeCfg.IncludeFreshUnplaced = true
	probeCfg.BypassRunawayBreaker = true
	probeCfg.RestrictToReset = false

	slog.Info("launching auto-probe",
		"component", "relaunch", "job_id", probeJob.ID,
		"interval", interval, "since_gate", now.Sub(time.Unix(gateAt, 0)).Truncate(time.Second))

	result, err := RelaunchOrphanedJobs(probeCfg)
	if err != nil {
		return 0, fmt.Errorf("auto-probe relaunch: %w", err)
	}
	if result == nil || len(result.InstanceIDs) == 0 {
		return 0, nil
	}
	probeLaunchID := result.InstanceIDs[0]
	_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchAutoProbeLaunched,
		LaunchID:  probeLaunchID,
		Detail:    runawayScopeDetail(cfg.ScopeProject, fmt.Sprintf("launch_id=%d job_id=%d interval=%s", probeLaunchID, probeJob.ID, interval)),
	})
	return probeLaunchID, nil
}

// latestAutoProbeLaunchID returns the launch_id of the most recent
// auto-probe event (0 if none).
func latestAutoProbeLaunchID(database *sql.DB) (int64, error) {
	row := database.QueryRow(
		`SELECT launch_id FROM lifecycle_events
		 WHERE event_kind = ? AND launch_id IS NOT NULL
		 ORDER BY occurred_at DESC LIMIT 1`,
		db.EventRelaunchAutoProbeLaunched,
	)
	var id sql.NullInt64
	if err := row.Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// pickAutoProbeJob returns one cheap eligible unplaced job for use as a
// probe. Cheapness proxy: lowest GPU memory requirement, breaking ties
// by job ID. Returns (nil, nil) if no eligible job.
func pickAutoProbeJob(cfg RelaunchConfig) (*db.Job, error) {
	if cfg.Database == nil {
		return nil, nil
	}
	jobs, err := db.ListUnplacedJobs(cfg.Database)
	if err != nil {
		return nil, err
	}
	scope := make(map[int64]bool, len(cfg.ScopeJobIDs))
	for _, id := range cfg.ScopeJobIDs {
		scope[id] = true
	}
	var picked *db.Job
	for _, j := range jobs {
		if j == nil || j.HasTag(db.TagInventory) {
			continue
		}
		if len(scope) > 0 && !scope[j.ID] {
			continue
		}
		if cfg.ScopeProject != "" && j.Project != cfg.ScopeProject {
			continue
		}
		if picked == nil || autoProbeJobLess(j, picked) {
			picked = j
		}
	}
	return picked, nil
}

func autoProbeJobLess(a, b *db.Job) bool {
	am, bm := 0, 0
	if a.GPUMemGB != nil {
		am = *a.GPUMemGB
	}
	if b.GPUMemGB != nil {
		bm = *b.GPUMemGB
	}
	if am != bm {
		return am < bm
	}
	return a.ID < b.ID
}
