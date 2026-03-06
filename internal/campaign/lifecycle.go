package campaign

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	weftsync "github.com/osteele/weft/internal/sync"
)

// LaunchOpts configures an instance launch.
type LaunchOpts struct {
	MaxSpendCents  int
	MaxTimeSeconds int
	SourceDir      string // Local project directory to rsync to cloud instance
	UseAgent       bool   // Deploy agent binary for job execution (vs bash wrapper)
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

	// Build wrapper script (agent-based or legacy)
	var wrapper string
	if opts.UseAgent {
		var agentJobs []cloud.AgentJob
		for _, job := range group.Jobs {
			agentJobs = append(agentJobs, cloud.AgentJob{
				ID:      job.ID,
				Command: job.EffectiveCommand(),
				Dir:     job.EffectiveWorkingDir(),
			})
		}
		wrapper = cloud.GenerateAgentWrapper(client, agentJobs, r2Cfg.Bucket)
	} else {
		var campaignJobs []cloud.CampaignJob
		for _, job := range group.Jobs {
			campaignJobs = append(campaignJobs, cloud.CampaignJob{
				ID:      job.ID,
				Command: job.EffectiveCommand(),
			})
		}
		wrapper = cloud.GenerateCampaignWrapper(client, instanceID, campaignJobs, r2Cfg.Bucket, false)
	}

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
	inst, err = client.WaitReady(providerInstID, 5*time.Minute)
	if err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("wait ready: %w", err)
	}

	// Record when instance became ready (before SSH setup)
	_ = db.SetCloudInstanceReadyAt(database, instanceID)

	sshTarget := fmt.Sprintf("root@%s", inst.SSHHost)
	sshPort := fmt.Sprintf("%d", inst.SSHPort)
	sshOpts := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-p", sshPort}

	// Deploy rclone config
	progress("configuring R2")
	rcloneConf := cloud.GenerateRcloneConfig(r2Cfg)
	setupCmd := fmt.Sprintf("mkdir -p ~/.config/rclone && cat > ~/.config/rclone/rclone.conf << 'RCLONE_EOF'\n%sRCLONE_EOF", rcloneConf)
	if _, err := cloud.SSHRun(sshTarget, sshOpts, setupCmd); err != nil {
		_ = client.DestroyInstance(providerInstID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("write rclone config: %w", err)
	}

	// Deploy agent binary and sync sources (if using agent)
	if opts.UseAgent {
		progress("deploying agent + syncing sources")

		// Build the agent binary (cached locally)
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

		if opts.SourceDir != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sshCmd := fmt.Sprintf("ssh -p %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null", sshPort)
				syncErr = weftsync.SyncSourcesWithSSH(sshTarget, opts.SourceDir, client.WorkspacePath(), sshCmd)
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
	}

	// Deploy wrapper script
	progress("deploying wrapper")
	wsPath := client.WorkspacePath()
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
