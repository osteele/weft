package terminal

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

func summaryInterval(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < 2*time.Minute:
		return 30 * time.Second
	case elapsed < 10*time.Minute:
		return 1 * time.Minute
	default:
		return 5 * time.Minute
	}
}

// watchInstancesPlain prints line-oriented status updates for cloud instances.
// Suitable for non-TTY output and parsing by coding agents.
func watchInstancesPlain(database *sql.DB, mode watchMode, instanceIDs []int64, estimateSummary *campaign.CostEstimateSummary, projectFilter string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, _ := config.Load()
	r2Client, _ := buildR2Client(cfg)

	campaignID, launchTime := campaignInfoFromInstances(database, instanceIDs)

	var estimateLine string
	if estimateSummary != nil {
		estimateLine = estimateSummary.FormatLine()
	}

	updates := make(map[int64]campaign.InstanceUpdate, len(instanceIDs))
	for _, id := range instanceIDs {
		ci, _ := db.GetLaunch(database, id)
		jobs, _ := db.GetLaunchJobsIncludingAttempts(database, id)
		outcomes, _ := db.GetAttemptOutcomesByLaunch(database, id)
		updates[id] = campaign.InstanceUpdate{
			Launch:             ci,
			Jobs:               jobs,
			JobAttemptOutcomes: outcomes,
		}
	}

	// Load initial unplaced jobs
	unplacedJobs, _ := db.ListUnplacedJobs(database)
	unplacedJobs = filterInstanceModeUnplacedJobs(unplacedJobs, projectFilter)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var lastSummaryTime time.Time

	// Print header
	if campaignID > 0 {
		switch mode {
		case watchModeCampaign:
			fmt.Printf("Campaign %d — launched %s\n\n", campaignID, launchTime.Format("2006-01-02 15:04"))
		case watchModeInstances:
			fmt.Printf("Launched %s\n\n", launchTime.Format("2006-01-02 15:04"))
		}
	}
	if summary := formatWatchSummaryLine(launchTime, campaignPlainViews(instanceIDs, updates), time.Now(), estimateLine); summary != "" {
		fmt.Println(summary)
		fmt.Println()
	}
	if len(unplacedJobs) > 0 {
		fmt.Print(formatUnplacedJobsSection(unplacedJobs))
		fmt.Println()
	}

	// Handle ctrl-c gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancel()
	}()

	// Periodic cloud instance reconciliation and job result sync.
	go func() {
		ticker := time.NewTicker(TerminalSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = syncCloudStateForTUI(database, false)
				_ = syncCloudStateForTUI(database, true)
				if refreshed, err := db.ListUnplacedJobs(database); err == nil {
					mu.Lock()
					unplacedJobs = filterInstanceModeUnplacedJobs(refreshed, projectFilter)
					mu.Unlock()
				}
			}
		}
	}()
	var retryMu sync.Mutex
	retryDone := false

	// startWatching launches a goroutine that watches a single instance and
	// prints updates. Used for both initial instances and relaunched ones.
	// Declared as a variable so the closure can reference itself for relaunch.
	var startWatching func(int64)
	startWatching = func(instanceID int64) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := clientForInstance(database, instanceID)
			ch := campaign.WatchInstance(ctx, client, database, instanceID, 2*time.Second, 10*time.Second, r2Client)
			var prev campaign.InstanceUpdate

			for update := range ch {
				output := campaign.FormatPlainUpdate(prev, update)
				mu.Lock()
				hasStateChange := output != ""
				if hasStateChange {
					fmt.Println(output)
				}
				updates[instanceID] = normalizeWatchInstanceUpdate(update, updates[instanceID].Launch)
				now := time.Now()
				if hasStateChange || now.Sub(lastSummaryTime) >= summaryInterval(now.Sub(launchTime)) {
					if summary := formatWatchSummaryLine(launchTime, campaignPlainViews(instanceIDs, updates), now, estimateLine); summary != "" {
						fmt.Println(summary)
						lastSummaryTime = now
					}
					if len(unplacedJobs) > 0 {
						fmt.Print(formatUnplacedJobsSection(unplacedJobs))
					}
				}
				mu.Unlock()

				// Auto-relaunch on retryable infrastructure failure with backoff
				ci := update.Launch
				if ci != nil && db.IsRetryableTermination(ci) {
					retryMu.Lock()
					shouldRetry := !retryDone
					if shouldRetry {
						retryDone = true
					}
					retryMu.Unlock()

					if shouldRetry {
						go func() {
							defer func() {
								retryMu.Lock()
								retryDone = false
								retryMu.Unlock()
							}()
							maxAttempts := len(retryBackoffDelays) + 1
							for attempt := range maxAttempts {
								fmt.Printf("instance %s: retryable failure (%s), attempting relaunch (attempt %d/%d)...\n",
									ids.FormatInstanceID(instanceID), ci.DisplayTerminationReason(), attempt+1, maxAttempts)
								scopeJobIDs := rentalScopedUnplacedJobIDs(unplacedJobs)
								outcome, err := attemptRelaunchOrphanedJobs(database, cfg, 0, nil, scopeJobIDs, projectFilter, false, false)
								if err != nil {
									fmt.Printf("instance %s: relaunch failed: %v\n", ids.FormatInstanceID(instanceID), err)
									return
								}
								if outcome != nil && outcome.BudgetSkip > 0 && len(outcome.InstanceIDs) == 0 {
									fmt.Printf("instance %s: %d job(s) exceeded retry budget, giving up\n", ids.FormatInstanceID(instanceID), outcome.BudgetSkip)
									return
								}
								if outcome != nil && outcome.BlockedReason != "" && len(outcome.InstanceIDs) == 0 {
									fmt.Printf("instance %s: auto-relaunch blocked: %s\n", ids.FormatInstanceID(instanceID), outcome.BlockedReason)
									return
								}
								if outcome != nil && outcome.Skipped > 0 && len(outcome.InstanceIDs) == 0 {
									fmt.Printf("instance %s: %d job(s) exceeded max cloud attempts, giving up\n", ids.FormatInstanceID(instanceID), outcome.Skipped)
									return
								}
								newIDs := outcome.InstanceIDs
								if len(newIDs) > 0 {
									fmt.Printf("instance %s: relaunched as instance(s) %s\n",
										ids.FormatInstanceID(instanceID), strings.Join(ids.FormatInstanceIDList(newIDs), ", "))
									for _, newID := range newIDs {
										startWatching(newID)
									}
									return
								}
								if attempt < len(retryBackoffDelays) {
									delay := retryBackoffDelays[attempt]
									fmt.Printf("instance %s: no offers available, retrying in %s...\n", ids.FormatInstanceID(instanceID), delay)
									select {
									case <-time.After(delay):
									case <-ctx.Done():
										return
									}
								}
							}
							fmt.Printf("instance %s: no offers available after %d attempts\n", ids.FormatInstanceID(instanceID), maxAttempts)
						}()
					}
				}

				prev = update
			}
		}()
	}

	for _, id := range instanceIDs {
		startWatching(id)
	}

	wg.Wait()
	return nil
}

func campaignPlainViews(instanceIDs []int64, updates map[int64]campaign.InstanceUpdate) []cloudInstanceView {
	views := make([]cloudInstanceView, 0, len(instanceIDs))
	for _, id := range instanceIDs {
		update := updates[id]
		views = append(views, cloudInstanceView{
			Launch:   update.Launch,
			Instance: update.Instance,
		})
	}
	return views
}

// clientForInstance creates a cloud.Client based on the provider stored in the DB.
func clientForInstance(database *sql.DB, instanceID int64) cloud.Client {
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil || ci == nil {
		return cloudClientForDBInstance("vastai") // fallback
	}
	return cloudClientForDBInstance(ci.Provider)
}

// campaignInfoFromInstances looks up campaign ID and launch time from the first instance.
func campaignInfoFromInstances(database *sql.DB, instanceIDs []int64) (campaignID int64, launchTime time.Time) {
	if len(instanceIDs) == 0 {
		return 0, time.Time{}
	}
	ci, err := db.GetLaunch(database, instanceIDs[0])
	if err != nil || ci == nil {
		return 0, time.Time{}
	}
	if ci.CampaignID != nil {
		campaignID = *ci.CampaignID
	}
	if ci.LaunchedAt != nil {
		launchTime = time.Unix(*ci.LaunchedAt, 0)
	} else {
		launchTime = time.Unix(ci.CreatedAt, 0)
	}
	return campaignID, launchTime
}
