package campaign

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/r2"
)

// DefaultMaxCloudAttempts is the default maximum number of cloud launch
// attempts per job before giving up.
const DefaultMaxCloudAttempts = 3

// RelaunchConfig configures automatic relaunch of orphaned cloud jobs.
type RelaunchConfig struct {
	Clients         []cloud.Client
	R2Cfg           cloud.R2Config
	CreateOpts      cloud.CreateOpts
	LaunchOpts      LaunchOpts
	MaxAttempts     int // default DefaultMaxCloudAttempts
	SurvivalModel   *bidding.SurvivalModel
	MinSurvival     float64 // 0 to disable survival filtering
	Strategy        bidding.SelectionStrategy
	Database        *sql.DB
	PredictorConfig *predictor.Config
	ResetJobs       map[int64]int64      // jobID → failed instanceID; when non-nil, only relaunch these jobs
	SetupFactory    SetupOverheadFactory // per-offer setup time estimator; use OfferSetupOverheadFactory to build
	RetryBudget     *RetryBudget         // optional hard stop limits for retry instances
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

// RelaunchResult holds the outcome of a relaunch pass.
type RelaunchResult struct {
	InstanceIDs []int64 // newly launched instance DB IDs
	Skipped     int     // jobs skipped (max attempts + budget limits + no offers)
	BudgetSkip  int     // jobs skipped due to retry budget limits
	Errors      []error
}

// RelaunchOrphanedJobs finds unplaced cloud jobs, filters by attempt count,
// groups them, fetches offers, and launches new instances. It reuses the
// campaign from the most recent attempt (if any) for cost inheritance.
func RelaunchOrphanedJobs(cfg RelaunchConfig) (*RelaunchResult, error) {
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxCloudAttempts
	}

	unplaced, err := db.ListUnplacedJobs(cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("list unplaced jobs: %w", err)
	}

	// If we know exactly which jobs were just orphaned, restrict to those.
	// This prevents user-deselected jobs from being swept into the relaunch.
	if len(cfg.ResetJobs) > 0 {
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
	result := &RelaunchResult{}
	var eligible []*db.Job
	for _, j := range unplaced {
		if j.HasTag(db.TagInventory) {
			continue
		}
		facts, err := attemptFactsForRelaunch(cfg.Database, j.ID)
		if err != nil {
			continue
		}
		count := facts.Count
		if count == 0 && !j.HasTag(db.TagRental) {
			continue
		}
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
			continue
		}
		if cfg.RetryBudget != nil && count > 0 {
			elapsed, spendCents := launchElapsedAndSpendCents(facts.LastLaunch, time.Now())
			if exceeded, detail := exceedsRetryBudget(*cfg.RetryBudget, count, elapsed, spendCents); exceeded {
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
				continue
			}
		}
		eligible = append(eligible, j)
	}

	if len(eligible) == 0 {
		return result, nil
	}

	slog.Info("eligible orphaned jobs for relaunch", "component", "relaunch", "count", len(eligible))
	_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchEligible,
		JobCount:  len(eligible),
	})

	// Group by GPU requirements and estimate disk
	groups := GroupByAffinity(eligible, nil)
	groups = SplitGroupsByImage(groups)
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
	for i := range groups {
		groups[i].DiskGB = EstimateGroupDisk(groups[i], cfg.Database, r2Client)
	}

	// If the most recent attempt for a group failed with disk_full, bump the
	// disk allocation so we don't retry a failing configuration. This only
	// applies within the automatic relaunch path — manual relaunches re-estimate
	// from scratch (the user may have edited the job or sources).
	for i, group := range groups {
		priorDisk := mostRecentDiskFullGB(cfg.Database, group)
		if priorDisk > 0 {
			floor := priorDisk + priorDisk/2 // 1.5x
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
	groupOffers := FetchGroupOffersWithPredictor(cfg.Clients, groups, cfg.PredictorConfig, cfg.SurvivalModel, cfg.SetupFactory, strategy, cfg.MinSurvival)

	// Filter to groups with valid offers
	var launchGroups []InstanceGroup
	var launchOffers []cloud.Offer
	for _, gOffer := range groupOffers {
		if gOffer.Offer == nil {
			slog.Warn("no offers for group, skipping", "component", "relaunch", "gpu_spec", gOffer.Group.GPUSpec(), "job_count", len(gOffer.Group.Jobs))
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchSkippedNoOffers,
				GPUSpec:   gOffer.Group.GPUSpec(),
				JobCount:  len(gOffer.Group.Jobs),
			})
			result.Skipped += len(gOffer.Group.Jobs)
			continue
		}
		if gOffer.Err != nil {
			slog.Warn("offer error for group", "component", "relaunch", "gpu_spec", gOffer.Group.GPUSpec(), "error", gOffer.Err)
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchSkippedOfferError,
				GPUSpec:   gOffer.Group.GPUSpec(),
				ErrorText: gOffer.Err.Error(),
			})
			result.Errors = append(result.Errors, gOffer.Err)
			continue
		}
		launchGroups = append(launchGroups, gOffer.Group)
		launchOffers = append(launchOffers, *gOffer.Offer)
	}

	if len(launchGroups) == 0 {
		return result, nil
	}

	// Prepare R2 assets (agent binary + source tarballs)
	r2Assets, err := PrepareR2Assets(cfg.R2Cfg, launchGroups)
	if err != nil {
		return result, fmt.Errorf("prepare R2 assets: %w", err)
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
			result.Errors = append(result.Errors, fmt.Errorf("relaunch: no client for provider %s", offer.Provider))
			continue
		}

		predecessorID, hasPredecessor := groupPredecessorIDs[i]
		wg.Add(1)
		go func(group InstanceGroup, offer cloud.Offer, client cloud.Client, predecessorID int64, hasPredecessor bool) {
			defer wg.Done()
			instanceID, err := LaunchInstance(
				client, cfg.Database, campaignID, group, offer,
				cfg.LaunchOpts, cfg.R2Cfg, cfg.CreateOpts,
				*r2Assets, nil, nil, nil,
			)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				slog.Warn("launch failed for group", "component", "relaunch", "gpu_spec", group.GPUSpec(), "error", err)
				_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
					EventKind: db.EventRelaunchLaunchFailed,
					GPUSpec:   group.GPUSpec(),
					JobCount:  len(group.Jobs),
					ErrorText: err.Error(),
				})
				result.Errors = append(result.Errors, fmt.Errorf("%s: %w", group.GPUSpec(), err))
				return
			}
			if hasPredecessor {
				if setErr := db.SetLaunchReplacedID(cfg.Database, instanceID, predecessorID); setErr != nil {
					slog.Warn("failed to set replaced_instance_id", "component", "relaunch", "instance", instanceID, "error", setErr)
				}
			}
			slog.Info("launched instance for relaunch", "component", "relaunch", "instance", instanceID, "job_count", len(group.Jobs), "gpu_spec", group.GPUSpec())
			_ = db.InsertLifecycleEvent(cfg.Database, &db.LifecycleEvent{
				EventKind: db.EventRelaunchLaunchSuccess,
				LaunchID:  instanceID,
				GPUSpec:   group.GPUSpec(),
				JobCount:  len(group.Jobs),
			})
			result.InstanceIDs = append(result.InstanceIDs, instanceID)
		}(group, offer, client, predecessorID, hasPredecessor)
	}
	wg.Wait()

	return result, nil
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

// countAttemptsForRelaunch returns the number of launch attempts for a job,
// scoped to the campaign of the job's most recent attempt. This prevents
// attempts from earlier campaigns from exhausting the retry budget.
type relaunchAttemptFacts struct {
	Count      int
	LastLaunch *db.Launch
}

func attemptFactsForRelaunch(database *sql.DB, jobID int64) (relaunchAttemptFacts, error) {
	attempts, err := db.GetLaunchAttempts(database, jobID)
	if err != nil || len(attempts) == 0 {
		return relaunchAttemptFacts{}, err
	}
	lastAttempt := attempts[len(attempts)-1]
	ci, err := db.GetLaunch(database, lastAttempt.LaunchID)
	if err != nil || ci == nil {
		count, countErr := db.CountLaunchAttempts(database, jobID)
		return relaunchAttemptFacts{Count: count, LastLaunch: nil}, countErr
	}
	if ci.CampaignID == nil {
		count, countErr := db.CountLaunchAttempts(database, jobID)
		return relaunchAttemptFacts{Count: count, LastLaunch: ci}, countErr
	}
	count, countErr := db.CountLaunchAttemptsInCampaign(database, jobID, *ci.CampaignID)
	return relaunchAttemptFacts{Count: count, LastLaunch: ci}, countErr
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
