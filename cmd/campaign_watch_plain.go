package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

// watchInstancesPlain prints line-oriented status updates for cloud instances.
// Suitable for non-TTY output and parsing by coding agents.
func watchInstancesPlain(database *sql.DB, instanceIDs []int64) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, _ := config.Load()
	r2Client, _ := buildR2Client(cfg)

	campaignID, launchTime := campaignInfoFromInstances(database, instanceIDs)

	updates := make(map[int64]campaign.InstanceUpdate, len(instanceIDs))
	for _, id := range instanceIDs {
		ci, _ := db.GetCloudInstance(database, id)
		jobs, _ := db.GetCloudInstanceJobsIncludingAttempts(database, id)
		outcomes, _ := db.GetAttemptOutcomesByInstance(database, id)
		updates[id] = campaign.InstanceUpdate{
			CloudInstance:      ci,
			Jobs:               jobs,
			JobAttemptOutcomes: outcomes,
		}
	}

	// Print campaign header if instances belong to a campaign
	if campaignID > 0 {
		fmt.Printf("Campaign %d — launched %s\n\n", campaignID, launchTime.Format("2006-01-02 15:04"))
	}
	if summary := formatCampaignWatchSummaryLine(launchTime, campaignPlainViews(instanceIDs, updates), time.Now()); summary != "" {
		fmt.Println(summary)
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

	// Periodic cloud instance reconciliation and job result sync (every 15s)
	reconciler := campaign.NewReconciler()
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cfg, _ := config.Load()
				syncCloudState(cfg, database, reconciler, false)
			}
		}
	}()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var prevSummary string
	var retried sync.Once

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
				if output != "" {
					fmt.Println(output)
				}
				updates[instanceID] = normalizeWatchInstanceUpdate(update, updates[instanceID].CloudInstance)
				if summary := formatCampaignWatchSummaryLine(launchTime, campaignPlainViews(instanceIDs, updates), time.Now()); summary != "" && summary != prevSummary {
					fmt.Println(summary)
					prevSummary = summary
				}
				mu.Unlock()

				// Auto-relaunch on retryable infrastructure failure (once across all instances)
				ci := update.CloudInstance
				if ci != nil && db.IsRetryableTermination(ci) {
					retried.Do(func() {
						fmt.Printf("instance %d: retryable failure (%s), attempting relaunch...\n", instanceID, ci.TerminationReason)
						newIDs, err := attemptRelaunchOrphanedJobs(database, cfg)
						if err != nil {
							fmt.Printf("instance %d: relaunch failed: %v\n", instanceID, err)
						} else if len(newIDs) > 0 {
							fmt.Printf("instance %d: relaunched as instance(s) %v\n", instanceID, newIDs)
							for _, newID := range newIDs {
								startWatching(newID)
							}
						}
					})
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
			CloudInstance: update.CloudInstance,
			Instance:      update.Instance,
		})
	}
	return views
}

// clientForInstance creates a cloud.Client based on the provider stored in the DB.
func clientForInstance(database *sql.DB, instanceID int64) cloud.Client {
	ci, err := db.GetCloudInstance(database, instanceID)
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
	ci, err := db.GetCloudInstance(database, instanceIDs[0])
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
