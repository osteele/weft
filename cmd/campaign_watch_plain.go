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

	// Print campaign header if instances belong to a campaign
	if campaignID, launchTime := campaignInfoFromInstances(database, instanceIDs); campaignID > 0 {
		fmt.Printf("Campaign %d — launched %s\n\n", campaignID, launchTime.Format("2006-01-02 15:04"))
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
	for _, id := range instanceIDs {
		wg.Add(1)
		go func(instanceID int64) {
			defer wg.Done()

			// Look up the provider from the DB to get the right client
			client := clientForInstance(database, instanceID)
			ch := campaign.WatchInstance(ctx, client, database, instanceID, 2*time.Second, 10*time.Second, r2Client)
			var prev campaign.InstanceUpdate

			for update := range ch {
				output := campaign.FormatPlainUpdate(prev, update)
				if output != "" {
					fmt.Println(output)
				}
				prev = update
			}
		}(id)
	}

	wg.Wait()
	return nil
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
