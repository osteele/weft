package campaign

import (
	"database/sql"
	"fmt"
	"log"
	"sync"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

// DefaultMaxCloudAttempts is the default maximum number of cloud launch
// attempts per job before giving up.
const DefaultMaxCloudAttempts = 3

// RelaunchConfig configures automatic relaunch of orphaned cloud jobs.
type RelaunchConfig struct {
	Clients     []cloud.Client
	R2Cfg       cloud.R2Config
	CreateOpts  cloud.CreateOpts
	LaunchOpts  LaunchOpts
	MaxAttempts int // default DefaultMaxCloudAttempts
	Database    *sql.DB
}

// RelaunchResult holds the outcome of a relaunch pass.
type RelaunchResult struct {
	InstanceIDs []int64 // newly launched instance DB IDs
	Skipped     int     // jobs exceeding max attempts
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

	// Single pass: filter to cloud jobs and check attempt count
	result := &RelaunchResult{}
	var eligible []*db.Job
	for _, j := range unplaced {
		if j.HasTag(db.TagInventory) {
			continue
		}
		count, err := db.CountJobCloudAttempts(cfg.Database, j.ID)
		if err != nil {
			continue
		}
		if count == 0 && !j.HasTag(db.TagRental) {
			continue
		}
		if count >= maxAttempts {
			log.Printf("relaunch: job %d has %d attempts (max %d), skipping", j.ID, count, maxAttempts)
			result.Skipped++
			continue
		}
		eligible = append(eligible, j)
	}

	if len(eligible) == 0 {
		return result, nil
	}

	log.Printf("relaunch: %d eligible orphaned jobs for relaunch", len(eligible))

	// Group by GPU requirements and estimate disk
	groups := GroupByGPUSupremum(eligible)
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
			log.Printf("relaunch: build R2 client for disk estimation: %v", err)
		}
	}
	for i := range groups {
		groups[i].DiskGB = EstimateGroupDisk(groups[i], cfg.Database, r2Client)
	}

	// Fetch offers
	groupOffers := FetchGroupOffers(cfg.Clients, groups, nil, 1.0, 0.5, bidding.StrategyCheap)

	// Filter to groups with valid offers
	var launchGroups []InstanceGroup
	var launchOffers []cloud.Offer
	for _, gOffer := range groupOffers {
		if gOffer.Offer == nil {
			log.Printf("relaunch: no offers for group %s, skipping %d jobs", gOffer.Group.GPUSpec(), len(gOffer.Group.Jobs))
			result.Skipped += len(gOffer.Group.Jobs)
			continue
		}
		if gOffer.Err != nil {
			log.Printf("relaunch: offer error for group %s: %v", gOffer.Group.GPUSpec(), gOffer.Err)
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
		attempts, err := db.GetJobCloudAttempts(cfg.Database, j.ID)
		if err != nil || len(attempts) == 0 {
			continue
		}
		lastAttempt := attempts[len(attempts)-1]
		ci, err := db.GetCloudInstance(cfg.Database, lastAttempt.CloudInstanceID)
		if err != nil || ci == nil || ci.CampaignID == nil {
			continue
		}
		campaignID = ci.CampaignID
		break
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

		wg.Add(1)
		go func(group InstanceGroup, offer cloud.Offer, client cloud.Client) {
			defer wg.Done()
			instanceID, err := LaunchInstance(
				client, cfg.Database, campaignID, group, offer,
				cfg.LaunchOpts, cfg.R2Cfg, cfg.CreateOpts,
				*r2Assets, nil, nil, nil,
			)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				log.Printf("relaunch: launch failed for group %s: %v", group.GPUSpec(), err)
				result.Errors = append(result.Errors, fmt.Errorf("%s: %w", group.GPUSpec(), err))
				return
			}
			log.Printf("relaunch: launched instance %d for %d jobs (group %s)", instanceID, len(group.Jobs), group.GPUSpec())
			result.InstanceIDs = append(result.InstanceIDs, instanceID)
		}(group, offer, client)
	}
	wg.Wait()

	return result, nil
}
