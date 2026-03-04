package campaign

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/vastai"
)

// LaunchOpts configures an instance launch.
type LaunchOpts struct {
	MaxSpendCents  int
	MaxTimeSeconds int
}

// LaunchInstance creates a cloud instance record, provisions a Vast.ai instance,
// deploys the multi-job wrapper, and starts it. Returns the cloud instance ID.
// If campaignID is non-nil, the cloud instance is associated with that campaign batch.
func LaunchInstance(
	client vastai.VastaiClient,
	database *sql.DB,
	campaignID *int64,
	group InstanceGroup,
	offer vastai.Offer,
	opts LaunchOpts,
	r2Cfg vastai.R2Config,
	createOpts vastai.CreateOpts,
	progress vastai.ProgressFunc,
) (int64, error) {
	if progress == nil {
		progress = func(string) {}
	}

	// Create cloud instance record
	instance := &db.CloudInstance{
		CampaignID:     campaignID,
		Status:         db.CloudInstanceStatusPlanned,
		Provider:       "vastai",
		GPUSpec:        group.GPUSpec(),
		GPUClass:       group.GPUClass,
		GPUMemGB:       group.GPUMemGB,
		MaxSpendCents:  opts.MaxSpendCents,
		MaxTimeSeconds: opts.MaxTimeSeconds,
	}
	instanceID, err := db.CreateCloudInstance(database, instance)
	if err != nil {
		return 0, fmt.Errorf("create cloud instance: %w", err)
	}

	// Associate jobs with cloud instance
	for _, job := range group.Jobs {
		if err := db.SetJobCloudInstanceID(database, job.ID, instanceID); err != nil {
			return instanceID, fmt.Errorf("set cloud_instance_id for job %d: %w", job.ID, err)
		}
	}

	// Update status to launching
	if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusLaunching); err != nil {
		return instanceID, fmt.Errorf("update instance status: %w", err)
	}

	// Build campaign wrapper
	var campaignJobs []vastai.CampaignJob
	for _, job := range group.Jobs {
		campaignJobs = append(campaignJobs, vastai.CampaignJob{
			ID:      job.ID,
			Command: job.EffectiveCommand(),
		})
	}
	wrapper := vastai.GenerateCampaignWrapper(instanceID, campaignJobs, r2Cfg.Bucket, false)

	// Create Vast.ai instance
	progress("creating instance")
	inst, err := client.CreateInstance(offer.ID, createOpts)
	if err != nil {
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("create instance: %w", err)
	}

	// Record Vast.ai instance ID
	if err := db.SetCloudInstanceVastaiID(database, instanceID, fmt.Sprintf("%d", inst.ID)); err != nil {
		_ = client.DestroyInstance(inst.ID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("record vastai instance ID: %w", err)
	}

	// Wait for instance ready
	progress("waiting for instance")
	vastaiInstanceID := inst.ID
	inst, err = client.WaitReady(vastaiInstanceID, 5*time.Minute)
	if err != nil {
		_ = client.DestroyInstance(vastaiInstanceID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("wait ready: %w", err)
	}

	sshTarget := fmt.Sprintf("root@%s", inst.SSHHost)
	sshPort := fmt.Sprintf("%d", inst.SSHPort)
	sshOpts := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-p", sshPort}

	// Deploy rclone config
	progress("configuring R2")
	rcloneConf := vastai.GenerateRcloneConfig(r2Cfg)
	setupCmd := fmt.Sprintf("mkdir -p ~/.config/rclone && cat > ~/.config/rclone/rclone.conf << 'RCLONE_EOF'\n%sRCLONE_EOF", rcloneConf)
	if _, err := vastai.SSHRun(sshTarget, sshOpts, setupCmd); err != nil {
		_ = client.DestroyInstance(inst.ID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("write rclone config: %w", err)
	}

	// Deploy wrapper script
	progress("deploying wrapper")
	deployCmd := fmt.Sprintf("cat > /workspace/.weft-campaign.sh << 'WRAPPER_EOF'\n%sWRAPPER_EOF\nchmod +x /workspace/.weft-campaign.sh", wrapper)
	if _, err := vastai.SSHRun(sshTarget, sshOpts, deployCmd); err != nil {
		_ = client.DestroyInstance(inst.ID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("deploy wrapper: %w", err)
	}

	// Start via nohup
	progress("starting jobs")
	startCmd := "nohup bash /workspace/.weft-campaign.sh </dev/null >/dev/null 2>&1 &"
	if _, err := vastai.SSHRun(sshTarget, sshOpts, startCmd); err != nil {
		_ = client.DestroyInstance(inst.ID)
		_ = db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed)
		return instanceID, fmt.Errorf("start wrapper: %w", err)
	}

	// Update status to running
	if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusRunning); err != nil {
		return instanceID, fmt.Errorf("update instance status: %w", err)
	}

	return instanceID, nil
}
