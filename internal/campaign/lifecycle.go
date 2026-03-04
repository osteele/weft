package campaign

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/vastai"
)

// LaunchOpts configures a campaign launch.
type LaunchOpts struct {
	MaxSpendCents  int
	MaxTimeSeconds int
}

// LaunchCampaign creates a campaign record, provisions a Vast.ai instance,
// deploys the multi-job wrapper, and starts it. Returns the campaign ID.
func LaunchCampaign(
	database *sql.DB,
	group CampaignGroup,
	offer vastai.Offer,
	opts LaunchOpts,
	r2Cfg vastai.R2Config,
	createOpts vastai.CreateOpts,
	progress vastai.ProgressFunc,
) (int64, error) {
	if progress == nil {
		progress = func(string) {}
	}

	// Create campaign record
	campaign := &db.Campaign{
		Status:         db.CampaignStatusPlanned,
		Provider:       "vastai",
		GPUSpec:        group.GPUSpec(),
		GPUClass:       group.GPUClass,
		GPUMemGB:       group.GPUMemGB,
		MaxSpendCents:  opts.MaxSpendCents,
		MaxTimeSeconds: opts.MaxTimeSeconds,
	}
	campaignID, err := db.CreateCampaign(database, campaign)
	if err != nil {
		return 0, fmt.Errorf("create campaign: %w", err)
	}

	// Associate jobs with campaign
	for _, job := range group.Jobs {
		if err := db.SetJobCampaignID(database, job.ID, campaignID); err != nil {
			return campaignID, fmt.Errorf("set campaign_id for job %d: %w", job.ID, err)
		}
	}

	// Update status to launching
	if err := db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusLaunching); err != nil {
		return campaignID, fmt.Errorf("update campaign status: %w", err)
	}

	// Build campaign wrapper
	var campaignJobs []vastai.CampaignJob
	for _, job := range group.Jobs {
		campaignJobs = append(campaignJobs, vastai.CampaignJob{
			ID:      job.ID,
			Command: job.EffectiveCommand(),
		})
	}
	wrapper := vastai.GenerateCampaignWrapper(campaignID, campaignJobs, r2Cfg.Bucket, false)

	// Create Vast.ai instance
	client := vastai.NewClient()

	progress("creating instance")
	inst, err := client.CreateInstance(offer.ID, createOpts)
	if err != nil {
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
		return campaignID, fmt.Errorf("create instance: %w", err)
	}

	// Record instance ID
	if err := db.SetCampaignInstanceID(database, campaignID, fmt.Sprintf("%d", inst.ID)); err != nil {
		_ = client.DestroyInstance(inst.ID)
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
		return campaignID, fmt.Errorf("record instance ID: %w", err)
	}

	// Wait for instance ready
	progress("waiting for instance")
	inst, err = client.WaitReady(inst.ID, 5*time.Minute)
	if err != nil {
		_ = client.DestroyInstance(inst.ID)
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
		return campaignID, fmt.Errorf("wait ready: %w", err)
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
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
		return campaignID, fmt.Errorf("write rclone config: %w", err)
	}

	// Deploy campaign wrapper
	progress("deploying campaign wrapper")
	deployCmd := fmt.Sprintf("cat > /workspace/.weft-campaign.sh << 'WRAPPER_EOF'\n%sWRAPPER_EOF\nchmod +x /workspace/.weft-campaign.sh", wrapper)
	if _, err := vastai.SSHRun(sshTarget, sshOpts, deployCmd); err != nil {
		_ = client.DestroyInstance(inst.ID)
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
		return campaignID, fmt.Errorf("deploy wrapper: %w", err)
	}

	// Start campaign via nohup
	progress("starting campaign")
	startCmd := "nohup bash /workspace/.weft-campaign.sh </dev/null >/dev/null 2>&1 &"
	if _, err := vastai.SSHRun(sshTarget, sshOpts, startCmd); err != nil {
		_ = client.DestroyInstance(inst.ID)
		_ = db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusFailed)
		return campaignID, fmt.Errorf("start campaign: %w", err)
	}

	// Update status to running
	if err := db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusRunning); err != nil {
		return campaignID, fmt.Errorf("update campaign status: %w", err)
	}

	return campaignID, nil
}
