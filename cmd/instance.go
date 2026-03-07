package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
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

func init() {
	rootCmd.AddCommand(instanceCmd)
	instanceCmd.AddCommand(instanceListCmd)
	instanceCmd.AddCommand(instanceStatusCmd)
	instanceCmd.AddCommand(instanceTerminateCmd)
	instanceCmd.AddCommand(instanceSSHCmd)

	instanceSSHCmd.Flags().BoolVar(&instanceSSHPrint, "print", false, "Print the SSH command instead of connecting")
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

		gpuSpec := ci.GPUSpec
		if gpuSpec == "" {
			gpuSpec = ci.GPUClass
		}
		fmt.Printf("Instance %d — %s — %s\n", ci.ID, gpuSpec, ci.Status)
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
