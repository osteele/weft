package campaign

import (
	"database/sql"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	weftsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

// LaunchOpts configures an instance launch.
type LaunchOpts struct {
	MaxSpendCents  int
	MaxTimeSeconds int
}

// LaunchResult holds the outcome of a campaign launch.
type LaunchResult struct {
	CampaignID  int64
	InstanceIDs []int64
	Errors      []error
}

// LaunchCampaign creates a campaign record and launches instances for each group
// in parallel. It collects results and updates the campaign status.
// The onPhase callback, if non-nil, is called with progress updates per group.
func LaunchCampaign(
	clients []cloud.Client,
	database *sql.DB,
	groups []InstanceGroup,
	offers []cloud.Offer, // parallel to groups
	opts LaunchOpts,
	r2Cfg cloud.R2Config,
	createOpts cloud.CreateOpts,
	onPhase func(group InstanceGroup, phase string),
) (*LaunchResult, error) {
	// Create campaign batch record
	campaignRec := &db.Campaign{
		Status: db.CampaignStatusLaunching,
	}
	campaignID, err := db.CreateCampaign(database, campaignRec)
	if err != nil {
		return nil, fmt.Errorf("create campaign: %w", err)
	}

	// Launch instances in parallel
	var mu sync.Mutex
	var instanceIDs []int64
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
				client, database, &campaignID, group, ofr, opts, r2Cfg, createOpts, progress,
			)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				launchErrors = append(launchErrors, fmt.Errorf("%s: %w", group.GPUSpec(), err))
			} else {
				instanceIDs = append(instanceIDs, cID)
			}
		}(g, offer)
	}

	wg.Wait()

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

// LaunchInstance creates a cloud instance record, provisions an instance via
// the given cloud client, deploys the multi-job wrapper, and starts it.
// Returns the cloud instance ID.
// If campaignID is non-nil, the cloud instance is associated with that campaign batch.
func LaunchInstance(
	client cloud.Client,
	database *sql.DB,
	campaignID *int64,
	group InstanceGroup,
	offer cloud.Offer,
	opts LaunchOpts,
	r2Cfg cloud.R2Config,
	createOpts cloud.CreateOpts,
	progress cloud.ProgressFunc,
) (int64, error) {
	if progress == nil {
		progress = func(string) {}
	}

	// Create cloud instance record with offer metadata
	instance := &db.CloudInstance{
		CampaignID:       campaignID,
		Status:           db.CloudInstanceStatusPlanned,
		Provider:         string(client.Provider()),
		GPUSpec:          group.GPUSpec(),
		GPUClass:         group.GPUClass,
		GPUMemGB:         group.GPUMemGB,
		MaxSpendCents:    opts.MaxSpendCents,
		MaxTimeSeconds:   opts.MaxTimeSeconds,
		ResolvedGPUName:  offer.GPUName,
		CostPerHourCents: int(offer.CostPerHour * 100),
		NumGPUs:          offer.NumGPUs,
		DLPerf:           offer.DLPerf,
		Reliability:      offer.Reliability,
		InetDownMbps:     offer.DownloadBandwidth,
		InetUpMbps:       offer.UploadBandwidth,
		CUDAVersion:      offer.CUDAVersion,
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
	// Each unique local source dir maps to workspace/<basename> on the remote.
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
	wrapper := cloud.GenerateAgentWrapper(client, agentJobs, r2Cfg.Bucket)

	// Create cloud instance
	progress("creating instance")
	inst, err := client.CreateInstance(offer.ProviderID, createOpts)
	if err != nil {
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("create instance: %w", err)
	}

	// Record provider instance ID
	if err := db.SetCloudInstanceProviderID(database, instanceID, inst.ProviderID); err != nil {
		_ = client.DestroyInstance(inst.ProviderID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("record provider instance ID: %w", err)
	}

	// Record data center if available
	if offer.DataCenter != "" {
		_ = db.SetCloudInstanceDataCenter(database, instanceID, offer.DataCenter)
	}

	// Wait for instance ready
	progress("waiting for instance")
	providerInstID := inst.ProviderID
	inst, err = client.WaitReady(providerInstID, 10*time.Minute)
	if err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("wait ready: %w", err)
	}

	// Record when instance became ready (before SSH setup)
	_ = db.SetCloudInstanceReadyAt(database, instanceID)

	sshTarget := fmt.Sprintf("root@%s", inst.SSHHost)
	sshPort := fmt.Sprintf("%d", inst.SSHPort)
	sshOpts := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=10", "-p", sshPort}

	// Deploy rclone config (retry SSH since sshd may not be ready immediately)
	progress("configuring R2")
	rcloneConf := cloud.GenerateRcloneConfig(r2Cfg)
	setupCmd := fmt.Sprintf("mkdir -p ~/.config/rclone && cat > ~/.config/rclone/rclone.conf << 'RCLONE_EOF'\n%sRCLONE_EOF", rcloneConf)
	if _, err := cloud.SSHRunWithRetry(sshTarget, sshOpts, setupCmd, 5*time.Minute); err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("write rclone config: %w", err)
	}

	// Build the agent binary (cached locally)
	progress("deploying agent + syncing sources")
	agentBinary, err := agentdeploy.EnsureBuilt("cloud", "linux", "amd64")
	if err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("build agent: %w", err)
	}

	// Deploy agent and sync sources in parallel
	var wg sync.WaitGroup
	var deployErr, syncErr error

	wg.Add(1)
	go func() {
		defer wg.Done()
		deployErr = agentdeploy.DeployToSSH(agentBinary, sshTarget, sshOpts, "/usr/local/bin/weft-agent")
	}()

	if len(localToRemote) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Create remote directories, then sync sources
			var mkdirs []string
			for _, remoteDir := range localToRemote {
				mkdirs = append(mkdirs, fmt.Sprintf("'%s'", remoteDir))
			}
			mkdirCmd := fmt.Sprintf("mkdir -p %s", strings.Join(mkdirs, " "))
			if _, err := cloud.SSHRun(sshTarget, sshOpts, mkdirCmd); err != nil {
				syncErr = fmt.Errorf("create remote dirs: %w", err)
				return
			}
			sshCmd := fmt.Sprintf("ssh -p %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null", sshPort)
			for localDir, remoteDir := range localToRemote {
				if err := weftsync.SyncSourcesWithSSH(sshTarget, localDir, remoteDir, sshCmd); err != nil {
					syncErr = err
					return
				}
			}
		}()
	}

	wg.Wait()

	if deployErr != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("deploy agent: %w", deployErr)
	}
	if syncErr != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("sync sources: %w", syncErr)
	}

	// Deploy wrapper script
	progress("deploying wrapper")
	deployCmd := fmt.Sprintf("cat > %s.weft-campaign.sh << 'WRAPPER_EOF'\n%sWRAPPER_EOF\nchmod +x %s.weft-campaign.sh", wsPath, wrapper, wsPath)
	if _, err := cloud.SSHRun(sshTarget, sshOpts, deployCmd); err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("deploy wrapper: %w", err)
	}

	// Start via nohup
	progress("starting jobs")
	startCmd := fmt.Sprintf("nohup bash %s.weft-campaign.sh </dev/null >/dev/null 2>&1 &", wsPath)
	if _, err := cloud.SSHRun(sshTarget, sshOpts, startCmd); err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("start wrapper: %w", err)
	}

	// Update status to running
	if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusRunning); err != nil {
		return instanceID, fmt.Errorf("update instance status: %w", err)
	}

	return instanceID, nil
}
