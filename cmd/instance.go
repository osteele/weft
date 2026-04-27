package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/osteele/weft/internal/util"
	"github.com/spf13/cobra"
)

var instanceCmd = &cobra.Command{
	Use:     "instance",
	Aliases: []string{"instances"},
	Short:   "Manage individual cloud GPU instances",
}

var instanceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all cloud instances",
	RunE:  runInstanceList,
}

var instanceStatusCmd = &cobra.Command{
	Use:   "status <id> [id...]",
	Short: "Show cloud instance status with uptime and cost",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runInstanceStatus,
}

var instanceInfoCmd = &cobra.Command{
	Use:   "info <id> [id...]",
	Short: "Show cloud instance status with uptime and cost",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runInstanceStatus,
}

var instanceTerminateCmd = &cobra.Command{
	Use:     "terminate <id> [id...]",
	Aliases: []string{"cancel"},
	Short:   "Terminate cloud instances and destroy their cloud instances",
	Args:    cobra.MinimumNArgs(1),
	RunE:    runInstanceTerminate,
}

var instanceSSHCmd = &cobra.Command{
	Use:   "ssh <id>",
	Short: "SSH into a cloud instance",
	Long: `Waits for the cloud instance to be ready, then connects
via SSH. Use --print to print the SSH command instead of connecting.`,
	Args: cobra.ExactArgs(1),
	RunE: runInstanceSSH,
}

var instanceSSHPrint bool
var instanceSubmitCommand string

var instanceSubmitCmd = &cobra.Command{
	Use:   "submit <instance-id> <job-id>",
	Short: "Resubmit a job to a cloud instance in grace period",
	Long: `Re-syncs sources and resubmits a job to a cloud instance that is in
grace period (waiting after job failure). Use --command to override the job command.`,
	Args: cobra.ExactArgs(2),
	RunE: runInstanceSubmit,
}

var instanceExtendCmd = &cobra.Command{
	Use:   "extend <instance-id> [duration]",
	Short: "Extend the grace period of a cloud instance",
	Long:  `Extends the grace period deadline. Default extension is 15m.`,
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runInstanceExtend,
}

var instanceReleaseCmd = &cobra.Command{
	Use:   "release <instance-id>",
	Short: "Release a cloud instance from grace period (clean self-destruct)",
	Long: `Signals the instance to write completion markers and self-destruct.
Unlike terminate, this does a clean shutdown with proper completion markers.`,
	Args: cobra.ExactArgs(1),
	RunE: runInstanceRelease,
}

var instanceCordonReason string

var instanceCordonCmd = &cobra.Command{
	Use:   "cordon <instance-id> [id...]",
	Short: "Mark an instance so autopilot stops routing new jobs to it",
	Long: `Cordon flags an instance as ineligible for reuse. The instance keeps
running and any active job continues to completion, but the autopilot and
explicit reuse paths will skip it when placing new jobs. Use 'uncordon' to
clear the flag.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runInstanceCordon,
}

var instanceUncordonCmd = &cobra.Command{
	Use:   "uncordon <instance-id> [id...]",
	Short: "Clear the cordon flag on an instance",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runInstanceUncordon,
}

var instanceWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch cloud instances, on-prem jobs, and unplaced jobs",
	Long: `Watch the full active system state.

In an interactive terminal this launches an instance-centric TUI. In plain mode
it prints periodic summaries of cloud instances, on-prem active jobs, and
unplaced jobs.`,
	RunE: runWatchCommand,
}

var instanceLaunchCmd = &cobra.Command{
	Use:     "launch",
	Aliases: []string{"run", "start"},
	Short:   "Interactively select and launch cloud instances for unplaceable jobs",
	Long:    campaignLaunchCmd.Long,
	RunE:    runCampaignLaunch,
}

func init() {
	rootCmd.AddCommand(instanceCmd)
	instanceCmd.AddCommand(instanceListCmd)
	instanceCmd.AddCommand(instanceStatusCmd)
	instanceCmd.AddCommand(instanceInfoCmd)
	instanceCmd.AddCommand(instanceTerminateCmd)
	instanceCmd.AddCommand(instanceSSHCmd)
	instanceCmd.AddCommand(instanceSubmitCmd)
	instanceCmd.AddCommand(instanceExtendCmd)
	instanceCmd.AddCommand(instanceReleaseCmd)
	instanceCmd.AddCommand(instanceCordonCmd)
	instanceCmd.AddCommand(instanceUncordonCmd)
	instanceCmd.AddCommand(instanceWatchCmd)
	instanceCmd.AddCommand(instanceLaunchCmd)
	instanceCmd.AddCommand(instanceDiagnoseCmd)

	instanceSSHCmd.Flags().BoolVar(&instanceSSHPrint, "print", false, "Print the SSH command instead of connecting")
	instanceSubmitCmd.Flags().StringVar(&instanceSubmitCommand, "command", "", "Override the job command")
	instanceCordonCmd.Flags().StringVar(&instanceCordonReason, "reason", "", "Optional human-readable reason (recorded with the cordon)")

	configureWatchFlags(instanceWatchCmd)
	addCampaignLaunchFlags(instanceLaunchCmd)
}

// newR2ClientFromConfig loads config and creates an R2 client.
// Returns an error if R2 is not configured.
func newR2ClientFromConfig() (*r2.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	client, err := buildR2Client(cfg)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("R2 not configured in ~/.config/weft/config.toml")
	}
	return client, nil
}

// getGraceInstance fetches a cloud instance and verifies it is in grace period.
func getGraceInstance(database *sql.DB, instanceID int64) (*db.Launch, error) {
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	if ci == nil {
		return nil, fmt.Errorf("instance %s not found", ids.FormatInstanceID(instanceID))
	}
	if ci.Status != db.LaunchStatusGrace {
		return nil, fmt.Errorf("instance %s is not in grace period (status: %s)", ids.FormatInstanceID(instanceID), ci.Status)
	}
	return ci, nil
}

func runInstanceList(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	instances, err := db.ListLaunches(database)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	if len(instances) == 0 {
		fmt.Println("No cloud instances.")
		return nil
	}

	jobCounts, _ := db.GetLaunchJobCounts(database)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tCAMPAIGN\tSTATUS\tPROVIDER\tTYPE\tGPU SPEC\tJOBS\tINSTANCE ID\tDATACENTER\tCREATED\tACTUAL COST\n")

	for _, inst := range instances {
		created := time.Unix(inst.CreatedAt, 0).Format("01/02 15:04")

		costStr := "—"
		if inst.ActualSpendCents > 0 {
			costStr = fmt.Sprintf("$%.2f", float64(inst.ActualSpendCents)/100)
		}

		gpuSpec := inst.DisplayGPUBrief()
		if gpuSpec == "" {
			gpuSpec = "—"
		}

		providerInstID := inst.EffectiveProviderID()
		if providerInstID == "" {
			providerInstID = "—"
		}

		dc := inst.DataCenter
		if dc == "" {
			dc = "—"
		}

		campaignStr := "—"
		if inst.CampaignID != nil {
			campaignStr = fmt.Sprintf("%d", *inst.CampaignID)
		}

		typeStr := inst.InstanceType
		if typeStr == "" {
			typeStr = "—"
		}

		statusStr := inst.Status
		if inst.Cordoned {
			statusStr += " [cordoned]"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			ids.FormatInstanceID(inst.ID), campaignStr, statusStr, inst.Provider, typeStr, gpuSpec, jobCounts[inst.ID], providerInstID, dc, created, costStr)
	}
	w.Flush()
	return nil
}

func runInstanceStatus(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	for i, arg := range args {
		if i > 0 {
			fmt.Println()
		}

		id, err := ids.ParseInstanceID(arg)
		if err != nil {
			return fmt.Errorf("invalid instance ID %q: %w", arg, err)
		}

		ci, err := db.GetLaunch(database, id)
		if err != nil {
			return fmt.Errorf("get instance %s: %w", ids.FormatInstanceID(id), err)
		}
		if ci == nil {
			fmt.Fprintf(os.Stderr, "Instance %s not found\n", ids.FormatInstanceID(id))
			continue
		}

		statusLabel := campaign.DisplayInstanceStatus(ci)
		fmt.Printf("Instance %s — %s — %s\n", ids.FormatInstanceID(ci.ID), ci.DisplayGPUBrief(), statusLabel)
		if ci.TerminationReason != "" {
			fmt.Printf("  Terminated: %s\n", ci.DisplayTerminationReason())
		}
		if ci.Status == db.LaunchStatusCompleted {
			switch {
			case ci.ResultsVerified != nil && !*ci.ResultsVerified:
				fmt.Printf("  Results: UNVERIFIED (upload was partial/failed)\n")
			case ci.ResultsVerified != nil && *ci.ResultsVerified:
				fmt.Printf("  Results: verified\n")
			}
		}
		fmt.Printf("  Provider: %s\n", ci.Provider)
		if line := formatRentalLine(ci); line != "" {
			fmt.Printf("  Rental:   %s\n", line)
		}

		// Cloud instance info
		providerInstID := ci.EffectiveProviderID()
		var inst *cloud.Instance
		client := cloudClientForDBInstance(ci.Provider)
		if providerInstID != "" {
			if !campaign.IsInstanceTerminal(ci.Status) {
				inst, _ = client.ShowInstance(providerInstID)
			}
			if inst != nil && inst.Status != "" {
				fmt.Printf("  Instance: %s (%s)\n", providerInstID, inst.Status)
			} else {
				fmt.Printf("  Instance: %s\n", providerInstID)
			}
		} else {
			fmt.Printf("  Instance: (provisioning...)\n")
		}

		if ci.DataCenter != "" {
			fmt.Printf("  Location: %s\n", ci.DataCenter)
		}

		// Agent version and live status
		var liveUpdate *campaign.InstanceUpdate
		if !campaign.IsInstanceTerminal(ci.Status) {
			if r2c, r2err := newR2ClientFromConfig(); r2err == nil && r2c != nil {
				watchCtx, watchCancel := context.WithTimeout(context.Background(), 3*time.Second)
				ch := campaign.WatchInstance(watchCtx, client, database, ci.ID, 100*time.Millisecond, 100*time.Millisecond, r2c)
				if update, ok := <-ch; ok {
					liveUpdate = &update
				}
				watchCancel()
			}
		}
		// Read agent version from DB-cached live state (persists after terminal)
		if liveState, err := db.GetLaunchLiveState(database, ci.ID); err == nil && liveState != nil && liveState.AgentVersion != "" {
			fmt.Printf("  Agent:    %s\n", liveState.AgentVersion)
		}

		obs := terminal.ObserveLaunch(ci, inst, time.Now())
		if obs.Uptime != nil {
			fmt.Printf("  Uptime:   %s\n", *obs.Uptime)
		}
		if obs.Cost != nil {
			fmt.Printf("  Cost:     $%.2f\n", *obs.Cost)
		}
		if obs.Rate != nil {
			fmt.Printf("  Rate:     $%.2f/hr\n", *obs.Rate)
		}
		// Jobs
		jobs, err := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		if err != nil {
			return fmt.Errorf("get instance %s jobs: %w", ids.FormatInstanceID(ci.ID), err)
		}
		if liveUpdate == nil {
			liveUpdate = &campaign.InstanceUpdate{
				Launch:   ci,
				Instance: inst,
				Jobs:     jobs,
			}
		} else {
			liveUpdate.Launch = ci
			if liveUpdate.Instance == nil {
				liveUpdate.Instance = inst
			}
			if len(liveUpdate.Jobs) == 0 {
				liveUpdate.Jobs = jobs
			}
		}
		// Backfill phase/bootstrap from DB-cached live state when the
		// one-shot WatchInstance call didn't return R2 data in time.
		if liveUpdate.InstancePhase == "" || liveUpdate.BootstrapStage == "" {
			if ls, err := db.GetLaunchLiveState(database, ci.ID); err == nil && ls != nil {
				if liveUpdate.InstancePhase == "" {
					liveUpdate.InstancePhase = ls.InstancePhase
				}
				if liveUpdate.BootstrapStage == "" {
					liveUpdate.BootstrapStage = ls.BootstrapStage
				}
				if liveUpdate.PhaseChangedAt == nil && ls.PhaseChangedAt != nil {
					t := time.Unix(*ls.PhaseChangedAt, 0)
					liveUpdate.PhaseChangedAt = &t
				}
			}
		}
		// Backfill adaptive bootstrap timeout for accurate display.
		if liveUpdate.BootstrapTerminateAfter == 0 && ci.Provider != "" {
			if survival, err := db.ComputeBootstrapSurvival(database, ci.Provider); err == nil && survival != nil {
				liveUpdate.BootstrapTerminateAfter = survival.TerminateAfter
				liveUpdate.BootstrapDurations = survival.Durations
			}
		}
		activity := terminal.FormatObservedActivity(*liveUpdate, time.Now())
		if activity.Bootstrap != "" {
			fmt.Printf("  Bootstrap: %s\n", activity.Bootstrap)
		}
		if activity.Phase != "" {
			fmt.Printf("  Phase:    %s\n", activity.Phase)
		}
		if liveUpdate != nil && liveUpdate.HeartbeatAge > 0 {
			fmt.Printf("  Heartbeat: %s ago\n", liveUpdate.HeartbeatAge.Truncate(time.Second))
		}
		if label := campaign.TerminationIntentLabel(ci.TerminationIntent); label != "" {
			fmt.Printf("  Cleanup: %s\n", label)
			if detail := campaign.TerminationIntentDetail(ci.TerminationIntent); detail != "" {
				fmt.Printf("  Cleanup status: %s\n", detail)
			}
		}
		if detail := ci.CordonDetail(); detail != "" {
			fmt.Printf("  Cordoned: %s\n", detail)
			if ci.CordonedAt != nil {
				fmt.Printf("  Cordoned at: %s\n", time.Unix(*ci.CordonedAt, 0).Format("01/02 15:04"))
			}
		}

		outcomes, _ := db.GetAttemptOutcomesByLaunch(database, ci.ID)
		if len(jobs) > 0 {
			fmt.Printf("  Jobs:\n")
			printInstanceJob := func(j *db.Job) {
				displayStatus := campaign.AttemptDisplayStatus(j, outcomes)
				desc := j.Description
				if desc == "" {
					desc = campaign.TruncateCommand(j.EffectiveCommand(), 50)
				}
				fmt.Printf("    %-6s %-12s %s\n", ids.FormatJobID(j.ID), displayStatus, desc)
				if campaign.IsJobTerminal(displayStatus) {
					if timings, err := db.GetJobPhaseTimings(database, j.ID); err == nil {
						if summary := terminal.FormatUploadSummary(timings); summary != "" {
							fmt.Printf("             uploads: %s\n", summary)
						}
					}
				}
				if displayStatus == db.StatusFailed || displayStatus == db.AttemptOutcomeFailed || displayStatus == db.AttemptOutcomeOrphaned {
					excerpt := diagnosisSummaryFromJSON(j.ErrorDiagnosis)
					if excerpt == "" {
						excerpt = terminal.ReadCachedJobFailureExcerpt(j.ID)
					}
					if excerpt == "" {
						excerpt = util.Truncate(strings.TrimSpace(strings.Join([]string{j.FailureReason, j.ErrorMessage, j.ErrorDiagnosis}, " | ")), 180)
					}
					if excerpt != "" {
						fmt.Printf("             failure: %s\n", excerpt)
					}
				}
			}
			for _, j := range jobs {
				printInstanceJob(j)
			}
		}

		// Previous instances (replacement chain)
		if donors := walkReplacementChain(database, ci); len(donors) > 0 {
			line := terminal.FormatPreviousInstanceLine(donors, time.Now())
			if line != "" {
				fmt.Println(line)
			}
		}
	}
	return nil
}

// walkReplacementChain follows ReplacedInstanceID links to build a predecessor chain.
func walkReplacementChain(database *sql.DB, ci *db.Launch) []*db.Launch {
	return terminal.CollectReplacementChain(ci, func(id int64) *db.Launch {
		predecessor, _ := db.GetLaunch(database, id)
		return predecessor
	})
}

// formatRentalLine renders the rental-type segment for `weft info`. Returns
// "" for pre-migration rows where InstanceType was never recorded.
func formatRentalLine(ci *db.Launch) string {
	switch ci.InstanceType {
	case cloud.InstanceTypeInterruptible:
		if ci.MaxBidPriceCents != nil {
			return fmt.Sprintf("interruptible · max bid $%.2f/hr", float64(*ci.MaxBidPriceCents)/100)
		}
		return "interruptible"
	case cloud.InstanceTypeOnDemand:
		return "on-demand"
	}
	return ""
}

func runInstanceTerminate(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var instanceIDs []int64
	for _, arg := range args {
		id, err := ids.ParseInstanceID(arg)
		if err != nil {
			return fmt.Errorf("invalid instance ID %q: %w", arg, err)
		}
		instanceIDs = append(instanceIDs, id)
	}

	terminated, errors := terminateInstancesParallel(database, instanceIDs)
	for _, e := range errors {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", e)
	}
	if terminated > 0 {
		fmt.Printf("Terminated %d instance(s)\n", terminated)
	}
	if len(errors) > 0 {
		return fmt.Errorf("%d instance(s) had errors", len(errors))
	}
	return nil
}

// terminateInstancesParallel terminates multiple cloud instances in parallel.
func terminateInstancesParallel(database *sql.DB, instanceIDs []int64) (int, []error) {
	return orchestration.TerminateInstancesParallel(database, instanceIDs)
}

func runInstanceSSH(cmd *cobra.Command, args []string) error {
	id, err := ids.ParseInstanceID(args[0])
	if err != nil {
		return fmt.Errorf("invalid instance ID %q: %w", args[0], err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	ci, err := db.GetLaunch(database, id)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}
	if ci == nil {
		return fmt.Errorf("instance %s not found", ids.FormatInstanceID(id))
	}

	providerInstID := ci.EffectiveProviderID()

	if providerInstID == "" {
		fmt.Println("Waiting for instance...")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		for providerInstID == "" {
			select {
			case <-ctx.Done():
				return fmt.Errorf("timed out waiting for instance")
			case <-time.After(2 * time.Second):
			}
			ci, err = db.GetLaunch(database, id)
			if err != nil {
				return fmt.Errorf("get instance: %w", err)
			}
			providerInstID = ci.EffectiveProviderID()
		}
	}

	// Get cloud instance info
	client := cloudClientForDBInstance(ci.Provider)

	// Wait for instance to be ready
	fmt.Println("Waiting for instance...")
	inst, err := client.WaitReady(providerInstID, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("wait for instance: %w", err)
	}

	fmt.Println("ready.")

	if instanceSSHPrint {
		fmt.Println(campaign.FormatSSHCommand(inst))
		return nil
	}

	return execSSH(inst)
}

func runInstanceSubmit(cmd *cobra.Command, args []string) error {
	instanceID, err := ids.ParseInstanceID(args[0])
	if err != nil {
		return fmt.Errorf("invalid instance ID %q: %w", args[0], err)
	}
	jobID, err := ids.ParseJobID(args[1])
	if err != nil {
		return fmt.Errorf("invalid job ID %q: %w", args[1], err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if _, err := getGraceInstance(database, instanceID); err != nil {
		return err
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}

	// Override command if specified via CLI flag
	if instanceSubmitCommand != "" {
		job.Command = instanceSubmitCommand
	}

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return err
	}

	ctx := context.Background()
	fmt.Printf("Re-syncing sources and submitting job %s to instance %s...\n", ids.FormatJobID(jobID), ids.FormatInstanceID(instanceID))

	if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, instanceID, []*db.Job{job}); err != nil {
		return err
	}

	// Persist command override to DB (SubmitJobsToInstance resets status to queued first)
	if instanceSubmitCommand != "" {
		if err := db.UpdateJobCommand(database, jobID, instanceSubmitCommand); err != nil {
			return fmt.Errorf("persist command override: %w", err)
		}
	}

	fmt.Printf("Job %s resubmitted to instance %s.\n", ids.FormatJobID(jobID), ids.FormatInstanceID(instanceID))
	fmt.Println("Use 'weft campaign watch' or 'weft instance status' to monitor progress.")
	return nil
}

func runInstanceExtend(cmd *cobra.Command, args []string) error {
	instanceID, err := ids.ParseInstanceID(args[0])
	if err != nil {
		return fmt.Errorf("invalid instance ID %q: %w", args[0], err)
	}

	duration := 15 * time.Minute
	if len(args) > 1 {
		d, err := time.ParseDuration(args[1])
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", args[1], err)
		}
		duration = d
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if _, err := getGraceInstance(database, instanceID); err != nil {
		return err
	}

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return err
	}

	ctx := context.Background()
	if _, err := controlplane.SendGraceExtend(ctx, r2Client, instanceID, duration); err != nil {
		return fmt.Errorf("write extend signal: %w", err)
	}

	newDeadline := time.Now().Add(duration).Unix()
	if err := db.ExtendLaunchGrace(database, instanceID, newDeadline); err != nil {
		return fmt.Errorf("update DB: %w", err)
	}

	fmt.Printf("Grace period for instance %s extended by %s.\n", ids.FormatInstanceID(instanceID), duration)
	return nil
}

func runInstanceRelease(cmd *cobra.Command, args []string) error {
	instanceID, err := ids.ParseInstanceID(args[0])
	if err != nil {
		return fmt.Errorf("invalid instance ID %q: %w", args[0], err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if _, err := getGraceInstance(database, instanceID); err != nil {
		return err
	}

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return err
	}

	ctx := context.Background()
	if _, err := controlplane.SendGraceRelease(ctx, r2Client, instanceID); err != nil {
		return fmt.Errorf("write release signal: %w", err)
	}

	if err := markReleasedInstanceFailed(database, instanceID); err != nil {
		return err
	}

	fmt.Printf("Instance %s released. It will self-destruct shortly.\n", ids.FormatInstanceID(instanceID))
	return nil
}

func runInstanceCordon(cmd *cobra.Command, args []string) error {
	return setInstanceCordon(args, true, instanceCordonReason)
}

func runInstanceUncordon(cmd *cobra.Command, args []string) error {
	return setInstanceCordon(args, false, "")
}

func setInstanceCordon(args []string, cordoned bool, reason string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	for _, arg := range args {
		id, err := ids.ParseInstanceID(arg)
		if err != nil {
			return fmt.Errorf("invalid instance ID %q: %w", arg, err)
		}
		err = db.SetLaunchCordoned(database, id, cordoned, reason)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("instance %s not found", ids.FormatInstanceID(id))
		}
		if err != nil {
			return fmt.Errorf("set cordon on %s: %w", ids.FormatInstanceID(id), err)
		}
		if !cordoned {
			fmt.Printf("Instance %s uncordoned.\n", ids.FormatInstanceID(id))
			continue
		}
		suffix := ""
		if reason != "" {
			suffix = " (" + reason + ")"
		}
		fmt.Printf("Instance %s cordoned%s. Autopilot will not route new jobs to it.\n", ids.FormatInstanceID(id), suffix)
	}
	return nil
}

func markReleasedInstanceFailed(database *sql.DB, instanceID int64) error {
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonJobFailure); err != nil {
		return fmt.Errorf("update DB: %w", err)
	}
	if _, err := db.NormalizeTerminalLaunchJobs(database, instanceID); err != nil {
		return fmt.Errorf("normalize jobs: %w", err)
	}
	return nil
}

func execSSH(inst *cloud.Instance) error {
	args := []string{
		"ssh",
		"-p", fmt.Sprintf("%d", inst.SSHPort),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		fmt.Sprintf("root@%s", inst.SSHHost),
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found: %w", err)
	}
	return syscall.Exec(sshPath, args, os.Environ())
}
