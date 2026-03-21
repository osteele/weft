package campaign

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	weftsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/vastai"
	"github.com/osteele/weft/internal/workdir"
)

// Cloud rental instances are currently always linux/amd64 (Vast.ai, RunPod).
const (
	cloudOS   = "linux"
	cloudArch = "amd64"
)

// LaunchOpts configures an instance launch.
type LaunchOpts struct {
	MaxSpendCents      int
	MaxTimeSeconds     int
	NoDonor            bool                      // skip donor instance strategy
	GracePeriodSeconds int                       // grace period after job failure (0 = disabled)
	Strategy           bidding.SelectionStrategy // "cheap" (default), "fast", or "fastest"
}

// ApplyAutoBudget derives budget limits from estimates for any limits not already set.
// Uses the maximum across all group estimates so no instance gets killed prematurely.
// Returns true if any limits were set.
func (opts *LaunchOpts) ApplyAutoBudget(estimates []CostEstimate) bool {
	if opts.MaxSpendCents > 0 && opts.MaxTimeSeconds > 0 {
		return false
	}

	var maxSpend, maxTime int
	for _, est := range estimates {
		spend, secs := BudgetFromEstimate(est)
		if spend > maxSpend {
			maxSpend = spend
		}
		if secs > maxTime {
			maxTime = secs
		}
	}

	changed := false
	if opts.MaxSpendCents == 0 && maxSpend > 0 {
		opts.MaxSpendCents = maxSpend
		changed = true
	}
	if opts.MaxTimeSeconds == 0 && maxTime > 0 {
		opts.MaxTimeSeconds = maxTime
		changed = true
	}
	return changed
}

// LaunchResult holds the outcome of a campaign launch.
type LaunchResult struct {
	CampaignID  int64
	InstanceIDs []int64
	Errors      []error
}

type stringPromise struct {
	done chan struct{}
	mu   sync.Mutex
	val  string
	err  error
}

func newStringPromise() *stringPromise {
	return &stringPromise{done: make(chan struct{})}
}

func (p *stringPromise) resolve(val string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return
	default:
		p.val = val
		p.err = err
		close(p.done)
	}
}

func (p *stringPromise) await() (string, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.val, p.err
}

// R2AssetStager uploads shared campaign assets in the background so group
// launches can start as soon as their own dependencies are ready.
type R2AssetStager struct {
	Client       *r2.Client
	AgentVersion string

	cancel         context.CancelFunc
	agentKey       *stringPromise
	sourcePromises map[string]*stringPromise
}

// StartR2AssetStaging starts uploading the agent binary and source tarballs to
// R2 in the background. Group launches can wait only on the directories they
// need instead of blocking on all assets globally.
func StartR2AssetStaging(r2Cfg cloud.R2Config, groups []InstanceGroup) (*R2AssetStager, error) {
	agentVersion, err := agentdeploy.LocalAgentVersion()
	if err != nil {
		return nil, fmt.Errorf("local agent version: %w", err)
	}

	r2Client, err := r2.New(r2.Config{
		AccountID:       r2Cfg.AccountID,
		AccessKeyID:     r2Cfg.AccessKeyID,
		SecretAccessKey: r2Cfg.SecretAccessKey,
		Bucket:          r2Cfg.Bucket,
	})
	if err != nil {
		return nil, fmt.Errorf("create R2 client: %w", err)
	}

	uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 10*time.Minute)

	stager := &R2AssetStager{
		Client:         r2Client,
		AgentVersion:   agentVersion,
		cancel:         uploadCancel,
		agentKey:       newStringPromise(),
		sourcePromises: make(map[string]*stringPromise),
	}

	allSourceDirs := make(map[string]bool)
	for _, g := range groups {
		for _, d := range g.SourceDirs() {
			allSourceDirs[d] = true
		}
	}
	for localDir := range allSourceDirs {
		stager.sourcePromises[localDir] = newStringPromise()
	}

	// Log periodic warnings so the user knows the upload is still in progress.
	uploadDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		elapsed := 15 * time.Second
		for {
			select {
			case <-uploadDone:
				return
			case <-ticker.C:
				log.Printf("R2 asset upload still in progress (%s elapsed)...", elapsed.Truncate(time.Second))
				elapsed += 15 * time.Second
			}
		}
	}()
	go func() {
		agentR2Key, err := agentdeploy.EnsureAgentInR2(uploadCtx, r2Client, agentVersion, cloudOS, cloudArch)
		stager.agentKey.resolve(agentR2Key, err)
	}()

	var uploadWg sync.WaitGroup
	for localDir, promise := range stager.sourcePromises {
		localDir := localDir
		promise := promise
		uploadWg.Add(1)
		go func() {
			defer uploadWg.Done()
			key, err := weftsync.UploadSourceToR2(uploadCtx, r2Client, localDir)
			if err != nil {
				oplog.Log(oplog.OpR2UploadSource,
					oplog.WithDetailf("dir: %s", localDir),
					oplog.WithError(err),
				)
				promise.resolve("", fmt.Errorf("upload source %s: %w", localDir, err))
				return
			}
			promise.resolve(key, nil)
		}()
	}

	go func() {
		_, _ = stager.agentKey.await()
		uploadWg.Wait()
		close(uploadDone)
	}()

	return stager, nil
}

func (s *R2AssetStager) Close() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

func (s *R2AssetStager) AwaitAssetsForDirs(dirs []string) (R2Assets, error) {
	if s == nil || s.Client == nil {
		return R2Assets{}, fmt.Errorf("R2 asset stager is not initialized")
	}
	agentR2Key, err := s.agentKey.await()
	if err != nil {
		return R2Assets{}, fmt.Errorf("upload agent to R2: %w", err)
	}
	assets := R2Assets{
		Client:       s.Client,
		AgentVersion: s.AgentVersion,
		AgentR2Key:   agentR2Key,
		SourceR2Keys: make(map[string]string),
	}
	for _, localDir := range dirs {
		promise, ok := s.sourcePromises[localDir]
		if !ok {
			continue
		}
		key, err := promise.await()
		if err != nil {
			return R2Assets{}, err
		}
		assets.SourceR2Keys[localDir] = key
	}
	return assets, nil
}

func (s *R2AssetStager) AwaitAll() (*R2Assets, error) {
	if s == nil {
		return nil, fmt.Errorf("R2 asset stager is not initialized")
	}
	dirs := make([]string, 0, len(s.sourcePromises))
	for localDir := range s.sourcePromises {
		dirs = append(dirs, localDir)
	}
	assets, err := s.AwaitAssetsForDirs(dirs)
	if err != nil {
		return nil, err
	}
	return &assets, nil
}

// PrepareR2Assets uploads the agent binary and source tarballs to R2,
// returning the pre-staged assets for use by LaunchInstance. This is
// extracted from LaunchCampaign so that RelaunchOrphanedJobs can reuse it.
func PrepareR2Assets(r2Cfg cloud.R2Config, groups []InstanceGroup) (*R2Assets, error) {
	stager, err := StartR2AssetStaging(r2Cfg, groups)
	if err != nil {
		return nil, err
	}
	defer stager.Close()
	return stager.AwaitAll()
}

type replacementOfferFunc func(failedOffer cloud.Offer) (*cloud.Offer, error)

const replacementOfferMaxPriceMultiplier = 1.25

func launchCostInputs(estimates []CostEstimate, idx int) (jobDurationHrs, setupOverheadHrs float64) {
	jobDurationHrs = 1.0
	setupOverheadHrs = 0.5
	if idx < 0 || idx >= len(estimates) {
		return jobDurationHrs, setupOverheadHrs
	}
	if runHours := estimates[idx].Breakdown.Run.Mean.Hours(); runHours > 0 {
		jobDurationHrs = runHours
	}
	if setupHours := estimates[idx].SetupOverhead.Hours(); setupHours > 0 {
		setupOverheadHrs = setupHours
	}
	return jobDurationHrs, setupOverheadHrs
}

func replacementOfferAllowed(failedOffer, replacementOffer cloud.Offer) bool {
	if failedOffer.CostPerHour <= 0 || replacementOffer.CostPerHour <= 0 {
		return true
	}
	return replacementOffer.CostPerHour <= failedOffer.CostPerHour*replacementOfferMaxPriceMultiplier
}

func createInstanceWithReplacement(
	client cloud.Client,
	group InstanceGroup,
	offer cloud.Offer,
	createOpts cloud.CreateOpts,
	progress cloud.ProgressFunc,
	updateOfferMetadata func(cloud.Offer) error,
	replacementOffer replacementOfferFunc,
) (*cloud.Instance, cloud.Offer, error) {
	currentOffer := offer

	progress("creating instance")
	inst, err := client.CreateInstance(currentOffer.ProviderID, createOpts)
	if err == nil {
		return inst, currentOffer, nil
	}
	if replacementOffer == nil || !errors.Is(err, cloud.ErrOfferUnavailable) {
		return nil, currentOffer, fmt.Errorf("create instance: %w", err)
	}

	log.Printf("launch: offer %s disappeared for %s; searching for replacement", currentOffer.ProviderID, group.GPUSpec())
	progress("offer disappeared; searching again")

	nextOffer, retryErr := replacementOffer(currentOffer)
	if retryErr != nil {
		return nil, currentOffer, fmt.Errorf("search replacement offer: %w", retryErr)
	}
	if nextOffer == nil {
		progress("offer disappeared; no replacement offer found")
		return nil, currentOffer, fmt.Errorf("offer %s disappeared and no replacement offer found", currentOffer.ProviderID)
	}

	if updateOfferMetadata != nil {
		if err := updateOfferMetadata(*nextOffer); err != nil {
			return nil, currentOffer, fmt.Errorf("update replacement offer metadata: %w", err)
		}
	}

	currentOffer = *nextOffer
	log.Printf("launch: retrying %s with replacement offer %s", group.GPUSpec(), currentOffer.ProviderID)
	progress("retrying with replacement offer")

	inst, err = client.CreateInstance(currentOffer.ProviderID, createOpts)
	if err != nil {
		return nil, currentOffer, fmt.Errorf("create instance: %w", err)
	}
	return inst, currentOffer, nil
}

// LaunchCampaign creates a campaign record and launches instances for each group
// in parallel. It collects results and updates the campaign status.
// The onPhase callback, if non-nil, is called with progress updates for campaign
// lifecycle steps and per-group launch activity.
func LaunchCampaign(
	clients []cloud.Client,
	database *sql.DB,
	groups []InstanceGroup,
	offers []cloud.Offer, // parallel to groups
	estimates []CostEstimate, // parallel to groups; may be nil
	survivalModel *bidding.SurvivalModel,
	opts LaunchOpts,
	r2Cfg cloud.R2Config,
	createOptsForProvider func(cloud.Provider) (cloud.CreateOpts, error),
	onPhase func(group InstanceGroup, phase string),
	onCampaignCreated func(id int64), // called after campaign record is created, before instances launch; may be nil
	onInstanceRegistered func(group InstanceGroup, instanceID int64),
) (*LaunchResult, error) {
	if len(groups) == 0 {
		return nil, fmt.Errorf("no instance groups to launch")
	}
	if len(offers) != len(groups) {
		return nil, fmt.Errorf("offers/groups mismatch: %d offers for %d groups", len(offers), len(groups))
	}

	if onPhase != nil {
		onPhase(InstanceGroup{GPUClass: "campaign"}, "preparing R2 assets")
	}
	stager, err := StartR2AssetStaging(r2Cfg, groups)
	if err != nil {
		return nil, err
	}
	defer stager.Close()

	// Compute total estimated cost from estimates
	var estimatedCostCents int
	if estimates != nil {
		totalCost := TotalEstimatedCostFromEstimates(estimates)
		estimatedCostCents = int(totalCost * 100)
	}

	// Create campaign batch record
	if onPhase != nil {
		onPhase(InstanceGroup{GPUClass: "campaign"}, "creating campaign record")
	}
	campaignRec := &db.Campaign{
		Status:             db.CampaignStatusLaunching,
		EstimatedCostCents: estimatedCostCents,
	}
	campaignID, err := db.CreateCampaign(database, campaignRec)
	if err != nil {
		return nil, fmt.Errorf("create campaign: %w", err)
	}

	if onCampaignCreated != nil {
		onCampaignCreated(campaignID)
	}

	// Donor strategy: find a cheap collocated instance for cache seeding
	var donorCfg *DonorConfig
	if !opts.NoDonor && len(groups) >= 2 {
		client := clientForProvider(clients, offers[0].Provider)
		if supportsDonorStrategy(client) {
			donorCfg, err = FindDonorOffer(client, offers, estimates, groups)
			if err != nil {
				log.Printf("donor: offer search failed, proceeding without donor: %v", err)
			}
		}
	}

	// Launch donor instance if strategy is available
	var donorInstanceID int64
	var donorProviderID string
	var donorClient cloud.Client
	if donorCfg != nil {
		donorClient = clientForProvider(clients, donorCfg.Offer.Provider)
		if donorClient != nil {
			if onPhase != nil {
				onPhase(InstanceGroup{GPUClass: "donor"}, "launching donor instance")
			}

			donorInst := &db.CloudInstance{
				CampaignID:       &campaignID,
				Status:           db.CloudInstanceStatusPlanned,
				Provider:         string(donorClient.Provider()),
				GPUSpec:          "donor",
				GPUClass:         donorCfg.Offer.GPUName,
				MaxSpendCents:    opts.MaxSpendCents,
				MaxTimeSeconds:   opts.MaxTimeSeconds,
				ResolvedGPUName:  donorCfg.Offer.GPUName,
				CostPerHourCents: int(donorCfg.Offer.CostPerHour * 100),
				NumGPUs:          donorCfg.Offer.NumGPUs,
				Reliability:      donorCfg.Offer.Reliability,
				InetDownMbps:     donorCfg.Offer.DownloadBandwidth,
				InetUpMbps:       donorCfg.Offer.UploadBandwidth,
				CUDAVersion:      donorCfg.Offer.CUDAVersion,
				InstanceRole:     "donor",
				MachineID:        donorCfg.Offer.MachineID,
			}
			var donorErr error
			donorInstanceID, donorErr = db.CreateCloudInstance(database, donorInst)
			if donorErr != nil {
				log.Printf("donor: failed to create DB record: %v", donorErr)
				donorCfg = nil
			} else {
				_ = db.SetCloudInstanceRole(database, donorInstanceID, "donor")

				// Generate donor bootstrap script
				donorAssets, donorAssetErr := stager.AwaitAssetsForDirs(donorCfg.SourceDirs)
				if donorAssetErr != nil {
					log.Printf("donor: staging assets failed: %v", donorAssetErr)
					donorCfg = nil
				}
				if donorCfg == nil {
					goto donorDisabled
				}

				var donorSources []SourceMapping
				for _, localDir := range donorCfg.SourceDirs {
					if r2Key, ok := donorAssets.SourceR2Keys[localDir]; ok {
						donorSources = append(donorSources, SourceMapping{
							R2Key:     r2Key,
							RemoteDir: path.Join(cloud.ProjectRootDir, path.Base(localDir)),
						})
					}
				}

				donorBootstrap := GenerateBootstrapScript(BootstrapManifest{
					AgentR2Key:   donorAssets.AgentR2Key,
					Sources:      donorSources,
					DonorMode:    true,
					HFModels:     donorCfg.HFModels,
					DonorID:      fmt.Sprintf("%d", donorInstanceID),
					DBInstanceID: donorInstanceID,
				})

				bootstrapKey := r2keys.BootstrapScript(donorInstanceID)
				donorUploadCtx, donorUploadCancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer donorUploadCancel()
				if uploadErr := donorAssets.Client.PutObject(donorUploadCtx, bootstrapKey, strings.NewReader(donorBootstrap), "text/x-shellscript"); uploadErr != nil {
					log.Printf("donor: failed to upload bootstrap: %v", uploadErr)
					donorCfg = nil
				} else {
					// Build env vars and create opts for donor
					donorCreateOpts := cloud.CreateOpts{}
					if createOptsForProvider != nil {
						donorCreateOpts, donorErr = createOptsForProvider(donorCfg.Offer.Provider)
						if donorErr != nil {
							log.Printf("donor: unsupported provider config: %v", donorErr)
							donorCfg = nil
						}
					}
					if donorCfg != nil {
						donorEnvVars := map[string]string{
							"R2_ACCESS_KEY_ID":     r2Cfg.AccessKeyID,
							"R2_SECRET_ACCESS_KEY": r2Cfg.SecretAccessKey,
							"R2_ENDPOINT":          donorAssets.Client.Endpoint(),
							"R2_BUCKET":            r2Cfg.Bucket,
						}
						if apiKey := vastai.ReadAPIKey(); apiKey != "" {
							donorEnvVars["VASTAI_API_KEY"] = apiKey
						}
						if token := os.Getenv("HF_TOKEN"); token != "" {
							donorEnvVars["HF_TOKEN"] = token
							donorEnvVars["HUGGING_FACE_HUB_TOKEN"] = token
						}
						donorCreateOpts.EnvVars = donorEnvVars
						if err := configureBootstrapCreateOpts(donorClient, &donorCreateOpts, bootstrapKey); err != nil {
							log.Printf("donor: bootstrap config failed: %v", err)
							donorCfg = nil
						}
					}
					if donorCfg != nil {
						donorCreateOpts.Label = fmt.Sprintf("weft/c%d", campaignID)

						inst, createErr := donorClient.CreateInstance(donorCfg.Offer.ProviderID, donorCreateOpts)
						if createErr != nil {
							log.Printf("donor: failed to create instance: %v", createErr)
							_ = db.UpdateCloudInstanceStatus(database, donorInstanceID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure)
							donorCfg = nil
						} else {
							donorProviderID = inst.ProviderID
							_ = db.SetCloudInstanceProviderID(database, donorInstanceID, donorProviderID)
							_ = db.UpdateCloudInstanceStatus(database, donorInstanceID, db.CloudInstanceStatusRunning)
							if donorCfg.Offer.DataCenter != "" {
								_ = db.SetCloudInstanceDataCenter(database, donorInstanceID, donorCfg.Offer.DataCenter)
							}
							log.Printf("donor: launched instance %s (DB ID %d) in %s", donorProviderID, donorInstanceID, donorCfg.DataCenter)
						}
					}
				}
			}
		}
	donorDisabled:
	}

	// Launch worker instances in parallel
	if onPhase != nil {
		onPhase(InstanceGroup{GPUClass: "campaign"}, "launching worker instances")
	}
	var mu sync.Mutex
	var instanceIDs []int64
	var workerProviderIDs []workerInfo
	var launchErrors []error
	var wg sync.WaitGroup

	for i, g := range groups {
		offer := offers[i]
		jobDurationHrs, setupOverheadHrs := launchCostInputs(estimates, i)

		wg.Add(1)
		go func(group InstanceGroup, ofr cloud.Offer, jobDurationHrs, setupOverheadHrs float64) {
			defer wg.Done()

			client := clientForProvider(clients, ofr.Provider)
			if client == nil {
				mu.Lock()
				launchErrors = append(launchErrors, fmt.Errorf("%s: no client for provider %s", group.GPUSpec(), ofr.Provider))
				mu.Unlock()
				return
			}

			var progress cloud.ProgressFunc
			if onPhase != nil {
				progress = func(phase string) {
					mu.Lock()
					onPhase(group, phase)
					mu.Unlock()
				}
			}
			var instanceRegistered func(int64)
			if onInstanceRegistered != nil {
				instanceRegistered = func(instanceID int64) {
					mu.Lock()
					onInstanceRegistered(group, instanceID)
					mu.Unlock()
				}
			}

			createOpts := cloud.CreateOpts{}
			if createOptsForProvider != nil {
				var err error
				createOpts, err = createOptsForProvider(ofr.Provider)
				if err != nil {
					mu.Lock()
					launchErrors = append(launchErrors, fmt.Errorf("%s: %w", group.GPUSpec(), err))
					mu.Unlock()
					return
				}
			}

			progress("waiting for R2 assets")
			groupAssets, assetErr := stager.AwaitAssetsForDirs(group.SourceDirs())
			if assetErr != nil {
				mu.Lock()
				launchErrors = append(launchErrors, fmt.Errorf("%s: %w", group.GPUSpec(), assetErr))
				mu.Unlock()
				return
			}

			replacementOffer := replacementOfferFunc(func(failedOffer cloud.Offer) (*cloud.Offer, error) {
				replacement := SearchBestOfferForGroup(
					[]cloud.Client{client},
					group,
					survivalModel,
					jobDurationHrs,
					setupOverheadHrs,
					map[string]struct{}{failedOffer.Key(): {}},
					opts.Strategy,
				)
				if replacement.Err != nil {
					return nil, replacement.Err
				}
				if replacement.Offer != nil && !replacementOfferAllowed(failedOffer, *replacement.Offer) {
					return nil, fmt.Errorf(
						"replacement offer price $%.2f/hr exceeds %.0f%% cap over expired offer $%.2f/hr",
						replacement.Offer.CostPerHour,
						(replacementOfferMaxPriceMultiplier-1)*100,
						failedOffer.CostPerHour,
					)
				}
				return replacement.Offer, nil
			})

			cID, err := LaunchInstance(
				client, database, &campaignID, group, ofr, opts, r2Cfg, createOpts,
				groupAssets, replacementOffer, progress, instanceRegistered,
			)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				launchErrors = append(launchErrors, fmt.Errorf("%s: %w", group.GPUSpec(), err))
			} else {
				instanceIDs = append(instanceIDs, cID)
				if donorCfg != nil {
					_ = db.SetCloudInstanceDonorID(database, cID, donorInstanceID)
					// Retrieve the provider ID for this instance
					inst, getErr := db.GetCloudInstance(database, cID)
					if getErr == nil && inst != nil {
						workerProviderIDs = append(workerProviderIDs, workerInfo{
							ProviderID: inst.EffectiveProviderID(),
							DBID:       cID,
						})
					}
				}
			}
		}(g, offer, jobDurationHrs, setupOverheadHrs)
	}

	wg.Wait()

	// Donor fan-out: wait for readiness, copy caches, then destroy donor
	if donorCfg != nil && donorProviderID != "" && len(workerProviderIDs) > 0 {
		if onPhase != nil {
			onPhase(InstanceGroup{GPUClass: "donor"}, "waiting for donor downloads")
		}

		donorReady := false
		readyKey := r2keys.DonorReady(donorInstanceID)
		downloadStart := time.Now()

		// Poll R2 for donor readiness
		for time.Since(downloadStart) < DefaultDonorReadyTimeout {
			exists, checkErr := stager.Client.ObjectExists(context.Background(), readyKey)
			if checkErr != nil {
				log.Printf("donor: R2 readiness check error: %v", checkErr)
			} else if exists {
				donorReady = true
				break
			}
			time.Sleep(10 * time.Second)
		}

		if donorReady {
			downloadSecs := int(time.Since(downloadStart).Seconds())
			_ = db.SetCloudInstanceSeedDownloadSecs(database, donorInstanceID, downloadSecs)
			log.Printf("donor: ready after %ds, starting fan-out to %d workers", downloadSecs, len(workerProviderIDs))

			if onPhase != nil {
				onPhase(InstanceGroup{GPUClass: "donor"}, "copying caches to workers")
			}

			var progressFunc func(int64, string)
			if onPhase != nil {
				progressFunc = func(dbID int64, phase string) {
					onPhase(InstanceGroup{GPUClass: "donor"}, fmt.Sprintf("worker %d: %s", dbID, phase))
				}
			}

			if seedErr := SeedWorkers(donorClient, database, donorProviderID, workerProviderIDs, DefaultDonorCachePaths, progressFunc); seedErr != nil {
				log.Printf("donor: fan-out had errors: %v", seedErr)
			}
		} else {
			log.Printf("donor: timed out waiting for readiness, workers will download independently")
		}

		// Destroy donor instance
		if onPhase != nil {
			onPhase(InstanceGroup{GPUClass: "donor"}, "destroying donor instance")
		}
		if destroyErr := donorClient.DestroyInstance(donorProviderID); destroyErr != nil {
			log.Printf("donor: failed to destroy: %v", destroyErr)
		}
		_ = db.UpdateCloudInstanceStatus(database, donorInstanceID, db.CloudInstanceStatusCompleted, db.TerminationReasonCompleted)
	}

	// Update campaign status
	if len(instanceIDs) == 0 && len(launchErrors) > 0 {
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
	} else {
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusRunning)
	}

	return &LaunchResult{
		CampaignID:  campaignID,
		InstanceIDs: instanceIDs,
		Errors:      launchErrors,
	}, nil
}

// clientForProvider finds the client matching a provider from a list.
func clientForProvider(clients []cloud.Client, provider cloud.Provider) cloud.Client {
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

func supportsDonorStrategy(client cloud.Client) bool {
	return client != nil && client.Provider() == cloud.ProviderVastai
}

func configureBootstrapCreateOpts(client cloud.Client, createOpts *cloud.CreateOpts, bootstrapKey string) error {
	switch client.Provider() {
	case cloud.ProviderRunpod:
		if createOpts.TemplateID == "" {
			return fmt.Errorf("runpod bootstrap requires a compatible template; run `weft runpod setup` or use startup command %q", cloud.R2BootstrapTemplateStartCmd())
		}
		if createOpts.EnvVars == nil {
			createOpts.EnvVars = make(map[string]string)
		}
		createOpts.EnvVars[cloud.R2BootstrapKeyEnvVar] = bootstrapKey
		createOpts.OnStartCmd = ""
	default:
		createOpts.OnStartCmd = cloud.R2BootstrapOnStartCmd(bootstrapKey)
	}
	return nil
}

// R2Assets holds pre-staged R2 resources shared across instances in a campaign.
type R2Assets struct {
	Client       *r2.Client
	AgentVersion string            // agent version string (jj commit hash)
	AgentR2Key   string            // R2 key for the agent binary
	SourceR2Keys map[string]string // localDir -> R2 key for source tarballs
}

// LaunchInstance creates a cloud instance record, pre-stages assets to R2, and
// creates a cloud instance that self-bootstraps from R2. No SSH is needed for
// setup — the instance downloads its bootstrap script via the onstart command.
// Returns the cloud instance DB ID.
func LaunchInstance(
	client cloud.Client,
	database *sql.DB,
	campaignID *int64,
	group InstanceGroup,
	offer cloud.Offer,
	opts LaunchOpts,
	r2Cfg cloud.R2Config,
	createOpts cloud.CreateOpts,
	r2Assets R2Assets,
	replacementOffer replacementOfferFunc,
	progress cloud.ProgressFunc,
	onInstanceRegistered func(instanceID int64),
) (int64, error) {
	if progress == nil {
		progress = func(string) {}
	}
	if r2Assets.Client == nil {
		return 0, fmt.Errorf("R2Assets.Client is required for R2-based bootstrap")
	}

	ctx := context.Background()

	// Create cloud instance record with offer metadata
	instance := &db.CloudInstance{
		CampaignID:        campaignID,
		Status:            db.CloudInstanceStatusPlanned,
		Provider:          string(client.Provider()),
		GPUSpec:           group.GPUSpec(),
		GPUClass:          group.GPUClass,
		GPUMemGB:          group.GPUMemGB,
		MaxSpendCents:     opts.MaxSpendCents,
		MaxTimeSeconds:    opts.MaxTimeSeconds,
		ResolvedGPUName:   offer.GPUName,
		CostPerHourCents:  int(offer.CostPerHour * 100),
		NumGPUs:           offer.NumGPUs,
		DLPerf:            offer.DLPerf,
		Reliability:       offer.Reliability,
		InetDownMbps:      offer.DownloadBandwidth,
		InetUpMbps:        offer.UploadBandwidth,
		CUDAVersion:       offer.CUDAVersion,
		DiskGB:            int(offer.DiskSpaceGB),
		ProvisionedInputs: group.AllInputs(),
		MachineID:         offer.MachineID,
	}
	instanceID, err := db.CreateCloudInstance(database, instance)
	if err != nil {
		return 0, fmt.Errorf("create cloud instance: %w", err)
	}

	// Associate jobs with cloud instance and record campaign position
	for i, job := range group.Jobs {
		if err := db.SetJobCloudInstanceID(database, job.ID, instanceID); err != nil {
			oplog.Log(oplog.OpCloudSetJobInstance,
				oplog.WithDetailf("job_id: %d, instance_id: %d", job.ID, instanceID),
				oplog.WithError(err),
			)
			return instanceID, fmt.Errorf("set cloud_instance_id for job %d: %w", job.ID, err)
		}
		updatedJob, err := db.GetJobByID(database, job.ID)
		if err != nil {
			return instanceID, fmt.Errorf("refresh job %d after cloud assignment: %w", job.ID, err)
		}
		if updatedJob != nil {
			group.Jobs[i] = updatedJob
		}
		if err := db.SetJobCampaignIndex(database, job.ID, i); err != nil {
			return instanceID, fmt.Errorf("set campaign_job_index for job %d: %w", job.ID, err)
		}
	}

	// Update status to launching
	if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusLaunching); err != nil {
		return instanceID, fmt.Errorf("update instance status: %w", err)
	}
	if onInstanceRegistered != nil {
		onInstanceRegistered(instanceID)
	}

	// Build local-to-remote directory mapping and agent job list.
	localToRemote := make(map[string]string)
	for _, d := range group.SourceDirs() {
		localToRemote[d] = path.Join(cloud.ProjectRootDir, path.Base(d))
	}

	var agentJobs []cloud.AgentJob
	for _, job := range group.Jobs {
		remoteDir := ""
		localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
		if mapped, ok := localToRemote[localDir]; ok {
			remoteDir = mapped
		}
		runID := int64(0)
		if job.LatestRunID != nil {
			runID = *job.LatestRunID
		}
		agentJobs = append(agentJobs, cloud.AgentJob{
			ID:      job.ID,
			RunID:   runID,
			Command: job.EffectiveCommand(),
			Dir:     remoteDir,
			Tags:    append([]string(nil), job.Tags...),
		})
	}

	// Override disk size if the group has a computed estimate
	if group.DiskGB > 0 && group.DiskGB > createOpts.DiskGB {
		createOpts.DiskGB = group.DiskGB
	}

	// Override image if the group has a per-project image
	if group.Image != "" {
		createOpts.Image = group.Image
	}

	// Build the bootstrap key using the DB instance ID (known before CreateInstance)
	bootstrapKey := r2keys.BootstrapScript(instanceID)

	// Build R2 env vars for the instance
	envVars := map[string]string{
		"R2_ACCESS_KEY_ID":     r2Cfg.AccessKeyID,
		"R2_SECRET_ACCESS_KEY": r2Cfg.SecretAccessKey,
		"R2_ENDPOINT":          r2Assets.Client.Endpoint(),
		"R2_BUCKET":            r2Cfg.Bucket,
	}
	// Pass Vast.ai API key for self-destruct (read from local config)
	if apiKey := vastai.ReadAPIKey(); apiKey != "" {
		envVars["VASTAI_API_KEY"] = apiKey
	}
	// Forward HF token for gated model downloads
	if token := os.Getenv("HF_TOKEN"); token != "" {
		envVars["HF_TOKEN"] = token
		envVars["HUGGING_FACE_HUB_TOKEN"] = token
	}

	// Merge env vars into createOpts
	if createOpts.EnvVars == nil {
		createOpts.EnvVars = envVars
	} else {
		for k, v := range envVars {
			createOpts.EnvVars[k] = v
		}
	}

	// Configure provider-specific bootstrap wiring.
	if err := configureBootstrapCreateOpts(client, &createOpts, bootstrapKey); err != nil {
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure)
		_, _ = db.ResetCloudInstanceJobs(database, instanceID, db.AttemptOutcomeOrphaned)
		return instanceID, fmt.Errorf("configure bootstrap: %w", err)
	}

	// Set instance label for provider dashboard visibility
	if campaignID != nil {
		createOpts.Label = fmt.Sprintf("weft/c%d", *campaignID)
	}

	campaignLogID := "nil"
	if campaignID != nil {
		campaignLogID = fmt.Sprintf("%d", *campaignID)
	}
	jobIDs := make([]string, 0, len(group.Jobs))
	for _, job := range group.Jobs {
		jobIDs = append(jobIDs, fmt.Sprintf("%d", job.ID))
	}
	oplog.Log(oplog.OpCloudInstanceLaunchRequested, oplog.WithDetailf(
		"cloud_instance_id=%d provider=%s campaign_id=%s offer_id=%s jobs=[%s] requested_disk_gb=%d base_disk_gb=%d group_disk_gb=%d offer_disk_gb=%.0f inputs=%d gpu=%s label=%s",
		instanceID,
		client.Provider(),
		campaignLogID,
		offer.ProviderID,
		strings.Join(jobIDs, ","),
		createOpts.DiskGB,
		cloud.DefaultCreateOpts("").DiskGB,
		group.DiskGB,
		offer.DiskSpaceGB,
		len(group.AllInputs()),
		group.GPUSpec(),
		createOpts.Label,
	))

	// Create cloud instance
	inst, finalOffer, err := createInstanceWithReplacement(
		client,
		group,
		offer,
		createOpts,
		progress,
		func(replacement cloud.Offer) error {
			return db.UpdateCloudInstanceOfferMetadata(database, instanceID, replacement)
		},
		replacementOffer,
	)
	if err != nil {
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure)
		_, _ = db.ResetCloudInstanceJobs(database, instanceID, db.AttemptOutcomeOrphaned)
		oplog.Log(oplog.OpCloudInstanceLaunchFailed, oplog.WithDetailf(
			"cloud_instance_id=%d provider=%s offer_id=%s error=%s",
			instanceID, client.Provider(), finalOffer.ProviderID, err,
		))
		return instanceID, err
	}

	providerInstID := inst.ProviderID
	oplog.Log(oplog.OpCloudInstanceLaunchCreated, oplog.WithDetailf(
		"cloud_instance_id=%d provider=%s provider_instance_id=%s requested_disk_gb=%d offer_id=%s status=%s",
		instanceID,
		client.Provider(),
		providerInstID,
		createOpts.DiskGB,
		finalOffer.ProviderID,
		inst.Status,
	))

	readback, readbackErr := client.ShowInstance(providerInstID)
	if readbackErr != nil {
		oplog.Log(oplog.OpCloudInstanceLaunchReadback,
			oplog.WithError(readbackErr),
			oplog.WithDetailf(
				"cloud_instance_id=%d provider=%s provider_instance_id=%s requested_disk_gb=%d",
				instanceID,
				client.Provider(),
				providerInstID,
				createOpts.DiskGB,
			),
		)
	} else {
		oplog.Log(oplog.OpCloudInstanceLaunchReadback, oplog.WithDetailf(
			"cloud_instance_id=%d provider=%s provider_instance_id=%s requested_disk_gb=%d provider_disk_gb=%.0f status=%s ssh_host=%s ssh_port=%d",
			instanceID,
			client.Provider(),
			providerInstID,
			createOpts.DiskGB,
			readback.DiskGB,
			readback.Status,
			readback.SSHHost,
			readback.SSHPort,
		))
		if createOpts.DiskGB > 0 && readback.DiskGB > 0 && math.Abs(readback.DiskGB-float64(createOpts.DiskGB)) >= 1 {
			oplog.Log(oplog.OpCloudInstanceLaunchMismatch, oplog.WithDetailf(
				"cloud_instance_id=%d provider=%s provider_instance_id=%s requested_disk_gb=%d provider_disk_gb=%.0f offer_disk_gb=%.0f",
				instanceID,
				client.Provider(),
				providerInstID,
				createOpts.DiskGB,
				readback.DiskGB,
				finalOffer.DiskSpaceGB,
			))
		}
	}

	// Record provider instance ID
	if err := db.SetCloudInstanceProviderID(database, instanceID, providerInstID); err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure)
		return instanceID, fmt.Errorf("record provider instance ID: %w", err)
	}

	// Record data center if available
	if finalOffer.DataCenter != "" {
		_ = db.SetCloudInstanceDataCenter(database, instanceID, finalOffer.DataCenter)
	}

	// Generate and upload bootstrap script (must happen after CreateInstance
	// so we have providerInstID for self-destruct, but before instance finishes
	// booting and runs onstart-cmd — boot typically takes several minutes).
	progress("uploading bootstrap")

	// Build source mappings for the bootstrap script
	var sources []SourceMapping
	for localDir, remoteDir := range localToRemote {
		if r2Key, ok := r2Assets.SourceR2Keys[localDir]; ok {
			sources = append(sources, SourceMapping{
				R2Key:     r2Key,
				RemoteDir: remoteDir,
			})
		}
	}

	// Store grace period in DB if configured
	if opts.GracePeriodSeconds > 0 {
		_ = db.SetCloudInstanceGracePeriod(database, instanceID, opts.GracePeriodSeconds)
	}

	// Generate and upload campaign manifest (needs providerInstID for self-destruct)
	selfDestructCmd := client.SelfDestructCmd(providerInstID)
	manifestJSON, err := cloud.GenerateCampaignManifest(agentJobs, selfDestructCmd, nil)
	if err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure)
		return instanceID, fmt.Errorf("generate campaign manifest: %w", err)
	}

	manifestKey := r2keys.CampaignManifest(instanceID)
	if err := r2Assets.Client.PutObject(ctx, manifestKey, bytes.NewReader(manifestJSON), "application/json"); err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure)
		return instanceID, fmt.Errorf("upload campaign manifest: %w", err)
	}

	// Store agent version for diagnostics
	if r2Assets.AgentVersion != "" {
		versionKey := r2keys.InstanceAgentVersion(instanceID)
		_ = r2Assets.Client.PutObject(ctx, versionKey, strings.NewReader(r2Assets.AgentVersion), "text/plain")
	}

	bootstrapScript := GenerateBootstrapScript(BootstrapManifest{
		AgentR2Key:         r2Assets.AgentR2Key,
		Sources:            sources,
		HFModels:           collectHFModels([]InstanceGroup{group}),
		DBInstanceID:       instanceID,
		MaxTimeSeconds:     opts.MaxTimeSeconds,
		GracePeriodSeconds: opts.GracePeriodSeconds,
	})

	if err := r2Assets.Client.PutObject(ctx, bootstrapKey, strings.NewReader(bootstrapScript), "text/x-shellscript"); err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure)
		return instanceID, fmt.Errorf("upload bootstrap script: %w", err)
	}

	// Update status to running — instance is now self-starting
	if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusRunning); err != nil {
		return instanceID, fmt.Errorf("update instance status: %w", err)
	}

	return instanceID, nil
}
