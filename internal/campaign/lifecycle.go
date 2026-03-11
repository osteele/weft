package campaign

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
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
	NoDonor            bool // skip donor instance strategy
	GracePeriodSeconds int  // grace period after job failure (0 = disabled)
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

// PrepareR2Assets uploads the agent binary and source tarballs to R2,
// returning the pre-staged assets for use by LaunchInstance. This is
// extracted from LaunchCampaign so that RelaunchOrphanedJobs can reuse it.
func PrepareR2Assets(r2Cfg cloud.R2Config, groups []InstanceGroup) (*R2Assets, error) {
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
	defer uploadCancel()

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
				log.Printf("agent upload still in progress (%s elapsed)...", elapsed.Truncate(time.Second))
				elapsed += 15 * time.Second
			}
		}
	}()
	agentR2Key, err := agentdeploy.EnsureAgentInR2(uploadCtx, r2Client, agentVersion, cloudOS, cloudArch)
	close(uploadDone)
	if err != nil {
		return nil, fmt.Errorf("upload agent to R2: %w", err)
	}

	assets := &R2Assets{
		Client:       r2Client,
		AgentVersion: agentVersion,
		AgentR2Key:   agentR2Key,
		SourceR2Keys: make(map[string]string),
	}

	// Pre-stage source tarballs to R2 (content-addressed, deduplicated)
	var sourceMu sync.Mutex
	var sourceWg sync.WaitGroup
	var sourceErr error

	allSourceDirs := make(map[string]bool)
	for _, g := range groups {
		for _, d := range g.SourceDirs() {
			allSourceDirs[d] = true
		}
	}
	for localDir := range allSourceDirs {
		localDir := localDir
		sourceWg.Add(1)
		go func() {
			defer sourceWg.Done()
			key, err := weftsync.UploadSourceToR2(uploadCtx, r2Client, localDir)
			sourceMu.Lock()
			defer sourceMu.Unlock()
			if err != nil {
				if sourceErr == nil {
					sourceErr = fmt.Errorf("upload source %s: %w", localDir, err)
				}
				return
			}
			assets.SourceR2Keys[localDir] = key
		}()
	}
	sourceWg.Wait()
	if sourceErr != nil {
		return nil, sourceErr
	}

	return assets, nil
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
	opts LaunchOpts,
	r2Cfg cloud.R2Config,
	createOpts cloud.CreateOpts,
	onPhase func(group InstanceGroup, phase string),
	onCampaignCreated func(id int64), // called after campaign record is created, before instances launch; may be nil
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
	r2Assets, err := PrepareR2Assets(r2Cfg, groups)
	if err != nil {
		return nil, err
	}

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
		if client != nil {
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
			}
			var donorErr error
			donorInstanceID, donorErr = db.CreateCloudInstance(database, donorInst)
			if donorErr != nil {
				log.Printf("donor: failed to create DB record: %v", donorErr)
				donorCfg = nil
			} else {
				_ = db.SetCloudInstanceRole(database, donorInstanceID, "donor")

				// Generate donor bootstrap script
				var donorSources []SourceMapping
				wsPath := donorClient.WorkspacePath()
				for _, localDir := range donorCfg.SourceDirs {
					if r2Key, ok := r2Assets.SourceR2Keys[localDir]; ok {
						donorSources = append(donorSources, SourceMapping{
							R2Key:     r2Key,
							RemoteDir: path.Join(wsPath, path.Base(localDir)),
						})
					}
				}

				donorBootstrap := GenerateBootstrapScript(BootstrapManifest{
					AgentR2Key:    r2Assets.AgentR2Key,
					Sources:       donorSources,
					WorkspacePath: wsPath,
					DonorMode:     true,
					HFModels:      donorCfg.HFModels,
					DonorID:       fmt.Sprintf("%d", donorInstanceID),
					DBInstanceID:  donorInstanceID,
				})

				bootstrapKey := r2keys.BootstrapScript(donorInstanceID)
				donorUploadCtx, donorUploadCancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer donorUploadCancel()
				if uploadErr := r2Assets.Client.PutObject(donorUploadCtx, bootstrapKey, strings.NewReader(donorBootstrap), "text/x-shellscript"); uploadErr != nil {
					log.Printf("donor: failed to upload bootstrap: %v", uploadErr)
					donorCfg = nil
				} else {
					// Build env vars and create opts for donor
					donorCreateOpts := createOpts
					donorEnvVars := map[string]string{
						"R2_ACCESS_KEY_ID":     r2Cfg.AccessKeyID,
						"R2_SECRET_ACCESS_KEY": r2Cfg.SecretAccessKey,
						"R2_ENDPOINT":          r2Assets.Client.Endpoint(),
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
					donorCreateOpts.OnStartCmd = cloud.R2BootstrapOnStartCmd(bootstrapKey)
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

		wg.Add(1)
		go func(group InstanceGroup, ofr cloud.Offer) {
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

			cID, err := LaunchInstance(
				client, database, &campaignID, group, ofr, opts, r2Cfg, createOpts,
				*r2Assets, progress,
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
		}(g, offer)
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
			exists, checkErr := r2Assets.Client.ObjectExists(context.Background(), readyKey)
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
	progress cloud.ProgressFunc,
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
	}
	instanceID, err := db.CreateCloudInstance(database, instance)
	if err != nil {
		return 0, fmt.Errorf("create cloud instance: %w", err)
	}

	// Associate jobs with cloud instance and record campaign position
	for i, job := range group.Jobs {
		if err := db.SetJobCloudInstanceID(database, job.ID, instanceID); err != nil {
			return instanceID, fmt.Errorf("set cloud_instance_id for job %d: %w", job.ID, err)
		}
		if err := db.SetJobCampaignIndex(database, job.ID, i); err != nil {
			return instanceID, fmt.Errorf("set campaign_job_index for job %d: %w", job.ID, err)
		}
	}

	// Update status to launching
	if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusLaunching); err != nil {
		return instanceID, fmt.Errorf("update instance status: %w", err)
	}

	// Build local-to-remote directory mapping and agent job list.
	wsPath := client.WorkspacePath()
	localToRemote := make(map[string]string)
	for _, d := range group.SourceDirs() {
		localToRemote[d] = path.Join(wsPath, path.Base(d))
	}

	var agentJobs []cloud.AgentJob
	for _, job := range group.Jobs {
		remoteDir := ""
		localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
		if mapped, ok := localToRemote[localDir]; ok {
			remoteDir = mapped
		}
		agentJobs = append(agentJobs, cloud.AgentJob{
			ID:      job.ID,
			Command: job.EffectiveCommand(),
			Dir:     remoteDir,
		})
	}

	// Override disk size if the group has a computed estimate
	if group.DiskGB > 0 && group.DiskGB > createOpts.DiskGB {
		createOpts.DiskGB = group.DiskGB
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

	// Set onstart command to bootstrap from R2
	createOpts.OnStartCmd = cloud.R2BootstrapOnStartCmd(bootstrapKey)

	// Set instance label for provider dashboard visibility
	if campaignID != nil {
		createOpts.Label = fmt.Sprintf("weft/c%d", *campaignID)
	}

	// Create cloud instance
	progress("creating instance")
	inst, err := client.CreateInstance(offer.ProviderID, createOpts)
	if err != nil {
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure)
		_, _ = db.ResetCloudInstanceJobs(database, instanceID, db.AttemptOutcomeOrphaned)
		return instanceID, fmt.Errorf("create instance: %w", err)
	}

	providerInstID := inst.ProviderID

	// Record provider instance ID
	if err := db.SetCloudInstanceProviderID(database, instanceID, providerInstID); err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure)
		return instanceID, fmt.Errorf("record provider instance ID: %w", err)
	}

	// Record data center if available
	if offer.DataCenter != "" {
		_ = db.SetCloudInstanceDataCenter(database, instanceID, offer.DataCenter)
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
		WorkspacePath:      wsPath,
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
