package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

var campaignCmd = &cobra.Command{
	Use:     "campaign",
	Aliases: []string{"campaigns"},
	Short:   "Manage cloud GPU campaigns (batches of instances)",
}

var campaignLaunchCmd = &cobra.Command{
	Use:     "launch",
	Aliases: []string{"run", "start"},
	Short:   "Interactively select and launch cloud instances for unplaceable jobs",
	Long:    instanceLaunchLong,
	RunE:    runInstanceLaunch,
}

var campaignWatchCmd = &cobra.Command{
	Use:   "watch [campaign-id]",
	Short: "Watch campaign instance progress",
	Long: `Monitors all instances in a campaign.

Defaults to TUI in an interactive terminal, otherwise plain text.
If no campaign ID is provided, watches the most recent campaign.
Use --tui or --plain to override.`,
	Args: usageArgs(cobra.MaximumNArgs(1)),
	RunE: runCampaignWatch,
}

var campaignTerminateCmd = &cobra.Command{
	Use:     "terminate <campaign-id>",
	Aliases: []string{"cancel"},
	Short:   "Terminate all instances in a campaign",
	Args:    cobra.ExactArgs(1),
	RunE:    runCampaignTerminate,
}

var campaignListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all campaigns with instance counts",
	RunE:  runCampaignList,
}

var campaignShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show details of a campaign",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runCampaignShow,
}

var (
	campaignWatchTUI   bool
	campaignWatchPlain bool
	campaignListTUI    bool
	campaignListPlain  bool
)

func init() {
	rootCmd.AddCommand(campaignCmd)
	campaignCmd.AddCommand(campaignLaunchCmd)
	campaignCmd.AddCommand(campaignWatchCmd)
	campaignCmd.AddCommand(campaignDiagnoseCmd)
	campaignCmd.AddCommand(campaignTerminateCmd)
	campaignCmd.AddCommand(campaignListCmd)
	campaignCmd.AddCommand(campaignShowCmd)
	campaignCmd.AddCommand(campaignStatsCmd)

	addInstanceLaunchFlags(campaignLaunchCmd)
	addCampaignWatchFlags(campaignWatchCmd)
	addCampaignListFlags(campaignListCmd)
}

func addCampaignWatchFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&campaignWatchTUI, "tui", false, "Force interactive TUI display")
	cmd.Flags().BoolVar(&campaignWatchPlain, "plain", false, "Force plain text output")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

func addCampaignListFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&campaignListTUI, "tui", false, "Force interactive list with drill-down to watch")
	cmd.Flags().BoolVar(&campaignListPlain, "plain", false, "Force plain table output")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

func resolveCampaignWatchID(database *sql.DB, args []string) (int64, error) {
	if len(args) > 0 {
		return parseCampaignID(args[0])
	}

	c, err := db.GetMostRecentCampaign(database)
	if err != nil {
		return 0, fmt.Errorf("get most recent campaign: %w", err)
	}
	if c == nil {
		return 0, fmt.Errorf("no campaigns found")
	}
	return c.ID, nil
}

func runCampaignWatch(cmd *cobra.Command, args []string) error {
	useTUI, err := resolveTUI(campaignWatchTUI, campaignWatchPlain)
	if err != nil {
		return err
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	campaignID, err := resolveCampaignWatchID(database, args)
	if err != nil {
		return err
	}

	instances, err := db.GetCampaignInstances(database, campaignID)
	if err != nil {
		return fmt.Errorf("get campaign instances: %w", err)
	}
	if len(instances) == 0 {
		return fmt.Errorf("campaign %d has no instances", campaignID)
	}

	var instanceIDs []int64
	for _, inst := range instances {
		instanceIDs = append(instanceIDs, inst.ID)
	}

	return watchAndReport(database, useTUI, terminal.ModeCampaign, instanceIDs, nil, "")
}

func runCampaignTerminate(cmd *cobra.Command, args []string) error {
	campaignID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid campaign ID %q: %w", args[0], err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	instances, err := db.GetCampaignInstances(database, campaignID)
	if err != nil {
		return fmt.Errorf("get campaign instances: %w", err)
	}
	if len(instances) == 0 {
		return fmt.Errorf("campaign %d has no instances", campaignID)
	}

	var ids []int64
	for _, inst := range instances {
		ids = append(ids, inst.ID)
	}

	terminated, errors := terminateInstancesParallel(database, ids)
	for _, e := range errors {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", e)
	}

	if err := db.UpdateCampaignStatus(database, campaignID, db.CampaignStatusCancelled); err != nil {
		return fmt.Errorf("update campaign status: %w", err)
	}

	fmt.Printf("Cancelled campaign %d, terminated %d instances\n", campaignID, terminated)
	return nil
}

func runCampaignList(cmd *cobra.Command, args []string) error {
	useTUI, err := resolveTUI(campaignListTUI, campaignListPlain)
	if err != nil {
		return err
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if !useTUI {
		reconcileBeforeDisplay(database, FastCloudSyncTimeout)
	}

	campaigns, err := db.ListCampaigns(database)
	if err != nil {
		return fmt.Errorf("list campaigns: %w", err)
	}

	if useTUI {
		return terminal.RunCampaignListTUI(database, campaigns)
	}

	if len(campaigns) == 0 {
		fmt.Println("No campaigns.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tSTATUS\tINSTANCES\tEST. COST\tACTUAL COST\tCREATED\n")

	for _, c := range campaigns {
		created := time.Unix(c.CreatedAt, 0).Format("01/02 15:04")
		instances, _ := db.GetCampaignInstances(database, c.ID)
		estCost := campaign.FormatEstimatedCostCents(c.EstimatedCostCents)
		actualCost := campaignActualCost(instances)
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\n",
			c.ID, c.Status, len(instances), estCost, actualCost, created)
	}
	w.Flush()
	return nil
}

func runCampaignShow(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	reconcileBeforeDisplay(database, FastCloudSyncTimeout)

	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid campaign ID %q: %w", args[0], err)
	}

	c, err := db.GetCampaign(database, id)
	if err != nil {
		return fmt.Errorf("get campaign: %w", err)
	}
	if c == nil {
		return fmt.Errorf("campaign %d not found", id)
	}

	fmt.Printf("Campaign %d\n", c.ID)
	fmt.Printf("  Status:     %s\n", c.Status)
	fmt.Printf("  Created:    %s\n", time.Unix(c.CreatedAt, 0).Format(time.RFC3339))
	if c.EndedAt != nil {
		fmt.Printf("  Ended:      %s\n", time.Unix(*c.EndedAt, 0).Format(time.RFC3339))
	}
	if c.DistinctMachines {
		covered, err := db.CampaignCoveredMachineIDs(database, c.ID)
		if err != nil {
			return fmt.Errorf("get covered machines: %w", err)
		}
		inflight, err := db.CampaignInflightMachineIDs(database, c.ID)
		if err != nil {
			return fmt.Errorf("get in-flight machines: %w", err)
		}
		fmt.Printf("  Distinct:   covered %d, in-flight %d, avoided %d\n", len(covered), len(inflight), len(c.AvoidMachines))
	}
	if len(c.AffinityMachines) > 0 {
		fmt.Printf("  Affinity:   %d machine(s)\n", len(c.AffinityMachines))
	}

	instances, err := db.GetCampaignInstances(database, c.ID)
	if err != nil {
		return fmt.Errorf("get campaign instances: %w", err)
	}

	if len(instances) > 0 {
		fmt.Printf("\n  Instances (%d):\n", len(instances))
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "    ID\tROLE\tSTATUS\tGPU SPEC\tPROVIDER ID\tTIMING\n")
		for _, inst := range instances {
			gpuSpec := inst.GPUSpec
			if gpuSpec == "" {
				gpuSpec = inst.GPUClass
			}
			providerID := inst.EffectiveProviderID()
			if providerID == "" {
				providerID = "—"
			}
			role := inst.InstanceRole
			if role == "" {
				role = "worker"
			}
			timing := "—"
			if inst.SeedDownloadSecs != nil {
				timing = fmt.Sprintf("download: %ds", *inst.SeedDownloadSecs)
			} else if inst.SeedCopySecs != nil {
				timing = fmt.Sprintf("copy: %ds", *inst.SeedCopySecs)
			}
			fmt.Fprintf(w, "    %d\t%s\t%s\t%s\t%s\t%s\n", inst.ID, role, inst.Status, gpuSpec, providerID, timing)
		}
		w.Flush()
	}

	return nil
}

func campaignActualCost(instances []*db.Launch) string {
	var totalCents float64
	for _, inst := range instances {
		if inst.CostPerHourCents == 0 || inst.LaunchedAt == nil {
			continue
		}
		var end time.Time
		if inst.EndedAt != nil {
			end = time.Unix(*inst.EndedAt, 0)
		} else {
			end = time.Now()
		}
		uptime := end.Sub(time.Unix(*inst.LaunchedAt, 0))
		totalCents += uptime.Hours() * float64(inst.CostPerHourCents)
	}
	return campaign.FormatCostCents(totalCents)
}
