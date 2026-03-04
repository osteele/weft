package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var campaignCmd = &cobra.Command{
	Use:   "campaign",
	Short: "Manage cloud GPU campaigns",
}

var campaignListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all campaigns",
	RunE:  runCampaignList,
}

var campaignShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show details of a campaign",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runCampaignShow,
}

func init() {
	rootCmd.AddCommand(campaignCmd)
	campaignCmd.AddCommand(campaignListCmd)
	campaignCmd.AddCommand(campaignShowCmd)
}

func runCampaignList(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	campaigns, err := db.ListCampaigns(database)
	if err != nil {
		return fmt.Errorf("list campaigns: %w", err)
	}

	if len(campaigns) == 0 {
		fmt.Println("No campaigns.")
		return nil
	}

	jobCounts, _ := db.GetCampaignJobCounts(database)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "ID\tSTATUS\tGPU SPEC\tJOBS\tCREATED\tACTUAL COST\n")

	for _, c := range campaigns {
		created := time.Unix(c.CreatedAt, 0).Format("01/02 15:04")

		costStr := "—"
		if c.ActualSpendCents > 0 {
			costStr = fmt.Sprintf("$%.2f", float64(c.ActualSpendCents)/100)
		}

		gpuSpec := c.GPUSpec
		if gpuSpec == "" {
			gpuSpec = c.GPUClass
		}

		fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\n",
			c.ID, c.Status, gpuSpec, jobCounts[c.ID], created, costStr)
	}
	w.Flush()
	return nil
}

func runCampaignShow(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var id int64
	if _, err := fmt.Sscanf(args[0], "%d", &id); err != nil {
		return fmt.Errorf("invalid campaign ID: %s", args[0])
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
	fmt.Printf("  Provider:   %s\n", c.Provider)
	fmt.Printf("  GPU Spec:   %s\n", c.GPUSpec)
	fmt.Printf("  Created:    %s\n", time.Unix(c.CreatedAt, 0).Format(time.RFC3339))
	if c.LaunchedAt != nil {
		fmt.Printf("  Launched:   %s\n", time.Unix(*c.LaunchedAt, 0).Format(time.RFC3339))
	}
	if c.EndedAt != nil {
		fmt.Printf("  Ended:      %s\n", time.Unix(*c.EndedAt, 0).Format(time.RFC3339))
	}
	if c.InstanceID != "" {
		fmt.Printf("  Instance:   %s\n", c.InstanceID)
	}
	if c.MaxSpendCents > 0 {
		fmt.Printf("  Max Spend:  $%.2f\n", float64(c.MaxSpendCents)/100)
	}
	if c.ActualSpendCents > 0 {
		fmt.Printf("  Actual:     $%.2f\n", float64(c.ActualSpendCents)/100)
	}

	// Show jobs
	jobs, err := db.GetCampaignJobs(database, c.ID)
	if err != nil {
		return fmt.Errorf("get campaign jobs: %w", err)
	}

	if len(jobs) > 0 {
		fmt.Printf("\n  Jobs (%d):\n", len(jobs))
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintf(w, "    ID\tSTATUS\tCOMMAND\n")
		for _, j := range jobs {
			cmd := truncate(j.EffectiveCommand(), 60)
			fmt.Fprintf(w, "    %d\t%s\t%s\n", j.ID, j.Status, cmd)
		}
		w.Flush()
	}

	return nil
}
