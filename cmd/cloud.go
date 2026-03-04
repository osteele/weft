package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/vastai"
	"github.com/spf13/cobra"
)

var cloudCmd = &cobra.Command{
	Use:   "cloud",
	Short: "Cloud GPU operations for unplaceable jobs",
}

var cloudPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Show groups of unplaceable jobs and estimated cloud costs",
	Long: `Groups needs_rental jobs by GPU requirements, queries Vast.ai for
the cheapest matching offer per group, and prints estimated costs.

Use 'weft cloud launch --group N' to launch a campaign for a specific group.`,
	RunE: runCloudPlan,
}

var cloudLaunchCmd = &cobra.Command{
	Use:   "launch",
	Short: "Launch a campaign for a group of unplaceable jobs",
	Long: `Provisions a Vast.ai instance and runs all jobs in the selected group
sequentially on it. Use 'weft cloud plan' first to see available groups.`,
	RunE: runCloudLaunch,
}

var (
	cloudLaunchGroup    int
	cloudLaunchMaxSpend string
	cloudLaunchMaxTime  string
)

func init() {
	rootCmd.AddCommand(cloudCmd)
	cloudCmd.AddCommand(cloudPlanCmd)
	cloudCmd.AddCommand(cloudLaunchCmd)

	cloudLaunchCmd.Flags().IntVar(&cloudLaunchGroup, "group", 0, "Group number from 'cloud plan' output (required)")
	cloudLaunchCmd.Flags().StringVar(&cloudLaunchMaxSpend, "max-spend", "", "Maximum spend (e.g., '$5.00')")
	cloudLaunchCmd.Flags().StringVar(&cloudLaunchMaxTime, "max-time", "", "Maximum time (e.g., '2h')")
	_ = cloudLaunchCmd.MarkFlagRequired("group")
}

func runCloudPlan(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	jobs, err := db.ListNeedsRentalJobs(database)
	if err != nil {
		return fmt.Errorf("list needs_rental jobs: %w", err)
	}

	if len(jobs) == 0 {
		fmt.Println("No jobs need rental GPUs.")
		return nil
	}

	groups := campaign.GroupByGPUSupremum(jobs)
	if len(groups) == 0 {
		fmt.Println("No jobs need rental GPUs.")
		return nil
	}

	client := vastai.NewClient()

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "GROUP\tGPU SPEC\tJOBS\tJOB IDS\tBEST OFFER\tCOST/HR\n")

	for i, g := range groups {
		jobIDs := formatJobIDs(g.Jobs)

		// Search Vast.ai for cheapest matching offer
		constraints := vastai.OfferConstraints{
			GPUClass:    g.GPUClass,
			MinGPUMemGB: g.GPUMemGB,
		}

		offers, err := client.SearchOffers(constraints)
		offerStr := "—"
		costStr := "—"
		if err != nil {
			offerStr = fmt.Sprintf("error: %v", err)
		} else if len(offers) > 0 {
			best := cheapestOffer(offers)
			offerStr = fmt.Sprintf("%s %dGB (id:%d)", best.GPUName, int(best.GPUMemGB), best.ID)
			costStr = fmt.Sprintf("$%.2f/hr", best.CostPerHour)
		} else {
			offerStr = "no offers found"
		}

		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\t%s\n",
			i+1, g.GPUSpec(), len(g.Jobs), jobIDs, offerStr, costStr)
	}
	w.Flush()

	fmt.Println()
	fmt.Println("To launch a campaign: weft cloud launch --group N")

	return nil
}

func formatJobIDs(jobs []*db.Job) string {
	if len(jobs) == 0 {
		return ""
	}
	result := ""
	for i, j := range jobs {
		if i > 0 {
			result += ","
		}
		result += fmt.Sprintf("%d", j.ID)
		if i >= 4 && len(jobs) > 5 {
			result += fmt.Sprintf(",…+%d", len(jobs)-5)
			break
		}
	}
	return result
}

func cheapestOffer(offers []vastai.Offer) vastai.Offer {
	best := offers[0]
	for _, o := range offers[1:] {
		if o.CostPerHour < best.CostPerHour {
			best = o
		}
	}
	return best
}

func runCloudLaunch(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return fmt.Errorf("R2 not configured in ~/.config/weft/config.yaml (vastai.r2)")
	}

	jobs, err := db.ListNeedsRentalJobs(database)
	if err != nil {
		return fmt.Errorf("list needs_rental jobs: %w", err)
	}

	groups := campaign.GroupByGPUSupremum(jobs)
	if cloudLaunchGroup < 1 || cloudLaunchGroup > len(groups) {
		return fmt.Errorf("invalid group %d (have %d groups, run 'weft cloud plan' to see them)", cloudLaunchGroup, len(groups))
	}

	group := groups[cloudLaunchGroup-1]

	// Search for best offer
	client := vastai.NewClient()
	if err := client.Available(); err != nil {
		return fmt.Errorf("vastai CLI not available: %w", err)
	}

	constraints := vastai.OfferConstraints{
		GPUClass:    group.GPUClass,
		MinGPUMemGB: group.GPUMemGB,
	}
	offers, err := client.SearchOffers(constraints)
	if err != nil {
		return fmt.Errorf("search offers: %w", err)
	}
	if len(offers) == 0 {
		return fmt.Errorf("no offers found for %s", group.GPUSpec())
	}
	best := cheapestOffer(offers)

	// Parse budget limits
	opts := campaign.LaunchOpts{}
	if cloudLaunchMaxSpend != "" {
		var dollars float64
		cleaned := cloudLaunchMaxSpend
		if len(cleaned) > 0 && cleaned[0] == '$' {
			cleaned = cleaned[1:]
		}
		if _, err := fmt.Sscanf(cleaned, "%f", &dollars); err == nil {
			opts.MaxSpendCents = int(dollars * 100)
		}
	}
	if cloudLaunchMaxTime != "" {
		if d, err := time.ParseDuration(cloudLaunchMaxTime); err == nil {
			opts.MaxTimeSeconds = int(d.Seconds())
		}
	}

	// Confirmation
	fmt.Printf("Campaign: %d jobs on %s\n", len(group.Jobs), group.GPUSpec())
	fmt.Printf("Best offer: %s %dGB at $%.2f/hr (id:%d)\n", best.GPUName, int(best.GPUMemGB), best.CostPerHour, best.ID)
	fmt.Printf("Job IDs: %s\n", formatJobIDs(group.Jobs))
	fmt.Print("Launch? [y/N] ")

	var confirm string
	fmt.Scanln(&confirm)
	if confirm != "y" && confirm != "Y" {
		fmt.Println("Cancelled.")
		return nil
	}

	r2Cfg := vastai.R2Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	}

	createOpts := vastai.CreateOpts{
		Image:      "nvidia/cuda:12.2-devel-ubuntu22.04",
		DiskGB:     50,
		SSHEnabled: true,
	}

	campaignID, err := campaign.LaunchCampaign(
		database, group, best, opts, r2Cfg, createOpts,
		func(phase string) {
			fmt.Printf("  %s...\n", phase)
		},
	)
	if err != nil {
		return fmt.Errorf("launch campaign: %w", err)
	}

	fmt.Printf("\nCampaign %d launched.\n", campaignID)
	fmt.Printf("Monitor with: weft campaign show %d\n", campaignID)
	return nil
}
