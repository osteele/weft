package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	weftsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
	"github.com/spf13/cobra"
)

var instanceCmd = &cobra.Command{
	Use:   "instance",
	Short: "Manage individual cloud GPU instances",
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

func init() {
	rootCmd.AddCommand(instanceCmd)
	instanceCmd.AddCommand(instanceListCmd)
	instanceCmd.AddCommand(instanceStatusCmd)
	instanceCmd.AddCommand(instanceTerminateCmd)
	instanceCmd.AddCommand(instanceSSHCmd)
	instanceCmd.AddCommand(instanceSubmitCmd)
	instanceCmd.AddCommand(instanceExtendCmd)
	instanceCmd.AddCommand(instanceReleaseCmd)

	instanceSSHCmd.Flags().BoolVar(&instanceSSHPrint, "print", false, "Print the SSH command instead of connecting")
	instanceSubmitCmd.Flags().StringVar(&instanceSubmitCommand, "command", "", "Override the job command")
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
		return nil, fmt.Errorf("R2 not configured in ~/.config/weft/config.yaml")
	}
	return client, nil
}

// getGraceInstance fetches a cloud instance and verifies it is in grace period.
func getGraceInstance(database *sql.DB, instanceID int64) (*db.CloudInstance, error) {
	ci, err := db.GetCloudInstance(database, instanceID)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	if ci == nil {
		return nil, fmt.Errorf("instance %d not found", instanceID)
	}
	if ci.Status != db.CloudInstanceStatusGrace {
		return nil, fmt.Errorf("instance %d is not in grace period (status: %s)", instanceID, ci.Status)
	}
	return ci, nil
}

func runInstanceList(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	instances, err := db.ListCloudInstances(database)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	if len(instances) == 0 {
		fmt.Println("No cloud instances.")
		return nil
	}

	jobCounts, _ := db.GetCloudInstanceJobCounts(database)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tSTATUS\tPROVIDER\tGPU SPEC\tJOBS\tINSTANCE ID\tDATACENTER\tCREATED\tACTUAL COST\n")

	for _, inst := range instances {
		created := time.Unix(inst.CreatedAt, 0).Format("01/02 15:04")

		costStr := "—"
		if inst.ActualSpendCents > 0 {
			costStr = fmt.Sprintf("$%.2f", float64(inst.ActualSpendCents)/100)
		}

		gpuSpec := inst.GPUSpec
		if gpuSpec == "" {
			gpuSpec = inst.GPUClass
		}

		providerInstID := inst.EffectiveProviderID()
		if providerInstID == "" {
			providerInstID = "—"
		}

		dc := inst.DataCenter
		if dc == "" {
			dc = "—"
		}

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			inst.ID, inst.Status, inst.Provider, gpuSpec, jobCounts[inst.ID], providerInstID, dc, created, costStr)
	}
	w.Flush()
	return nil
}

func runInstanceStatus(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	for i, arg := range args {
		if i > 0 {
			fmt.Println()
		}

		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid instance ID %q: %w", arg, err)
		}

		ci, err := db.GetCloudInstance(database, id)
		if err != nil {
			return fmt.Errorf("get instance %d: %w", id, err)
		}
		if ci == nil {
			fmt.Fprintf(os.Stderr, "Instance %d not found\n", id)
			continue
		}

		statusLabel := ci.Status
		if label := ci.GraceStatusLabel(); label != "" {
			statusLabel = label
		}
		fmt.Printf("Instance %d — %s — %s\n", ci.ID, ci.DisplayGPUSpec(), statusLabel)
		fmt.Printf("  Provider: %s\n", ci.Provider)

		// Cloud instance info
		providerInstID := ci.EffectiveProviderID()
		var inst *cloud.Instance
		if providerInstID != "" {
			client := cloudClientForDBInstance(ci.Provider)
			if !campaign.IsInstanceTerminal(ci.Status) {
				inst, _ = client.ShowInstance(providerInstID)
			}
			if inst != nil {
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

		// Uptime and cost
		if ci.LaunchedAt != nil {
			var uptime time.Duration
			if ci.EndedAt != nil {
				uptime = time.Duration(*ci.EndedAt-*ci.LaunchedAt) * time.Second
			} else {
				uptime = time.Since(time.Unix(*ci.LaunchedAt, 0))
			}
			uptime = uptime.Truncate(time.Minute)
			fmt.Printf("  Uptime:   %s\n", uptime)

			if inst != nil && inst.CostPerHour > 0 {
				cost := uptime.Hours() * inst.CostPerHour
				fmt.Printf("  Cost:     $%.2f ($%.2f/hr)\n", cost, inst.CostPerHour)
			} else if ci.ActualSpendCents > 0 {
				fmt.Printf("  Cost:     $%.2f\n", float64(ci.ActualSpendCents)/100)
			}
		}

		// Jobs
		jobs, err := db.GetCloudInstanceJobs(database, ci.ID)
		if err != nil {
			return fmt.Errorf("get instance %d jobs: %w", ci.ID, err)
		}
		if len(jobs) > 0 {
			completed := 0
			for _, j := range jobs {
				if j.Status == db.StatusCompleted || j.Status == db.StatusFailed {
					completed++
				}
			}
			fmt.Printf("  Jobs:     %d/%d completed\n", completed, len(jobs))
			for _, j := range jobs {
				desc := j.Description
				if desc == "" {
					desc = campaign.TruncateCommand(j.EffectiveCommand(), 50)
				}
				fmt.Printf("    %-6d %-12s %s\n", j.ID, j.Status, desc)
			}
		}
	}
	return nil
}

func runInstanceTerminate(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var ids []int64
	for _, arg := range args {
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid instance ID %q: %w", arg, err)
		}
		ids = append(ids, id)
	}

	terminated, errors := terminateInstancesParallel(database, ids)
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
func terminateInstancesParallel(database *sql.DB, ids []int64) (int, []error) {
	var mu sync.Mutex
	var terminated int
	var errors []error
	var wg sync.WaitGroup

	for _, id := range ids {
		wg.Add(1)
		go func(instanceID int64) {
			defer wg.Done()

			ci, err := db.GetCloudInstance(database, instanceID)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("get instance %d: %w", instanceID, err))
				mu.Unlock()
				return
			}
			if ci == nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("instance %d not found", instanceID))
				mu.Unlock()
				return
			}

			if campaign.IsInstanceTerminal(ci.Status) {
				mu.Lock()
				fmt.Printf("Instance %d already %s\n", instanceID, ci.Status)
				mu.Unlock()
				return
			}

			// Destroy cloud instance
			providerInstID := ci.EffectiveProviderID()
			if providerInstID != "" {
				client := cloudClientForDBInstance(ci.Provider)
				_ = client.DestroyInstance(providerInstID) // best-effort
			}

			if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusCancelled); err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("update instance %d status: %w", instanceID, err))
				mu.Unlock()
				return
			}

			resetCount, err := db.ResetCloudInstanceJobs(database, instanceID, db.AttemptOutcomeCancelled)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Errorf("reset jobs for instance %d: %w", instanceID, err))
				mu.Unlock()
				return
			}

			mu.Lock()
			terminated++
			instanceInfo := ""
			if providerInstID != "" {
				instanceInfo = fmt.Sprintf(", destroyed %s %s", ci.Provider, providerInstID)
			}
			fmt.Printf("Cancelled instance %d%s, %d jobs reset to needs_rental\n", instanceID, instanceInfo, resetCount)
			mu.Unlock()
		}(id)
	}

	wg.Wait()
	return terminated, errors
}

func runInstanceSSH(cmd *cobra.Command, args []string) error {
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid instance ID %q: %w", args[0], err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	ci, err := db.GetCloudInstance(database, id)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}
	if ci == nil {
		return fmt.Errorf("instance %d not found", id)
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
			ci, err = db.GetCloudInstance(database, id)
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
	instanceID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid instance ID %q: %w", args[0], err)
	}
	jobID, err := strconv.ParseInt(args[1], 10, 64)
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
		return fmt.Errorf("job %d not found", jobID)
	}

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return err
	}

	// Upload fresh sources
	sourceDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
	fmt.Printf("Re-syncing sources from %s...\n", sourceDir)
	ctx := context.Background()
	sourceR2Key, err := weftsync.UploadSourceToR2(ctx, r2Client, sourceDir)
	if err != nil {
		return fmt.Errorf("upload source: %w", err)
	}

	// Determine the command
	jobCmd := job.EffectiveCommand()
	if instanceSubmitCommand != "" {
		jobCmd = instanceSubmitCommand
	}

	// Write jobs.json to R2
	type gracePayload struct {
		Jobs    []cloud.AgentJob  `json:"jobs"`
		Sources map[string]string `json:"sources"`
	}

	payload := gracePayload{
		Jobs: []cloud.AgentJob{{
			ID:      jobID,
			Command: jobCmd,
		}},
		Sources: map[string]string{
			sourceDir: sourceR2Key,
		},
	}
	payloadJSON, _ := json.Marshal(payload)

	graceKey := r2keys.GraceJobs(instanceID)
	if err := r2Client.PutObject(ctx, graceKey, strings.NewReader(string(payloadJSON)), "application/json"); err != nil {
		return fmt.Errorf("write jobs.json to R2: %w", err)
	}

	if err := db.UpdateJobRunning(database, jobID); err != nil {
		return fmt.Errorf("update job status: %w", err)
	}

	fmt.Printf("Job %d resubmitted to instance %d.\n", jobID, instanceID)
	fmt.Println("Use 'weft campaign watch' or 'weft instance status' to monitor progress.")
	return nil
}

func runInstanceExtend(cmd *cobra.Command, args []string) error {
	instanceID, err := strconv.ParseInt(args[0], 10, 64)
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
	extendKey := r2keys.GraceExtend(instanceID)
	if err := r2Client.PutObject(ctx, extendKey, strings.NewReader(duration.String()), "text/plain"); err != nil {
		return fmt.Errorf("write extend signal: %w", err)
	}

	newDeadline := time.Now().Add(duration).Unix()
	if err := db.ExtendCloudInstanceGrace(database, instanceID, newDeadline); err != nil {
		return fmt.Errorf("update DB: %w", err)
	}

	fmt.Printf("Grace period for instance %d extended by %s.\n", instanceID, duration)
	return nil
}

func runInstanceRelease(cmd *cobra.Command, args []string) error {
	instanceID, err := strconv.ParseInt(args[0], 10, 64)
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
	releaseKey := r2keys.GraceRelease(instanceID)
	if err := r2Client.PutObject(ctx, releaseKey, strings.NewReader("release"), "text/plain"); err != nil {
		return fmt.Errorf("write release signal: %w", err)
	}

	// Grace period is entered because jobs failed, so releasing means giving up — mark as failed.
	if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed); err != nil {
		return fmt.Errorf("update DB: %w", err)
	}

	fmt.Printf("Instance %d released. It will self-destruct shortly.\n", instanceID)
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
