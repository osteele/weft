package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

var costCmd = &cobra.Command{
	Use:   "cost <instances|jobs|campaigns>",
	Short: "Show cost summaries for instances, jobs, or campaigns",
}

var costInstancesCmd = &cobra.Command{
	Use:     "instances",
	Aliases: []string{"instance"},
	Short:   "Show cost per cloud instance",
	RunE:    runCostInstances,
}

var costJobsCmd = &cobra.Command{
	Use:     "jobs",
	Aliases: []string{"job"},
	Short:   "Show cost per cloud job",
	RunE:    runCostJobs,
}

var costCampaignsCmd = &cobra.Command{
	Use:     "campaigns",
	Aliases: []string{"campaign"},
	Short:   "Show cost per campaign",
	RunE:    runCostCampaigns,
}

func init() {
	rootCmd.AddCommand(costCmd)
	costCmd.AddCommand(costInstancesCmd)
	costCmd.AddCommand(costJobsCmd)
	costCmd.AddCommand(costCampaignsCmd)

	// Noun-verb aliases: "weft instance cost", "weft job cost", "weft campaign cost"
	instanceCmd.AddCommand(verbAlias("cost", costInstancesCmd))
	jobCmd.AddCommand(verbAlias("cost", costJobsCmd))
	campaignCmd.AddCommand(verbAlias("cost", costCampaignsCmd))
}

func runCostInstances(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	reconcileBeforeDisplay(database, FastCloudSyncTimeout)

	instances, err := db.ListLaunches(database)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}

	var priced []*db.Launch
	for _, inst := range instances {
		if inst.CostPerHourCents > 0 {
			priced = append(priced, inst)
		}
	}
	if len(priced) == 0 {
		fmt.Println("No instances with pricing data.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Instance\tGPU\tStatus\tRate\tDuration\tCost\n")
	for _, inst := range priced {
		duration, actualCost := instanceCostBreakdown(inst)
		costPerHr := fmt.Sprintf("$%.2f/hr", float64(inst.CostPerHourCents)/100)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			ids.FormatInstanceID(inst.ID), inst.DisplayGPUBrief(), inst.Status,
			costPerHr, duration, actualCost)
	}
	w.Flush()
	return nil
}

func runCostJobs(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	jobs, err := db.ListJobsByStatuses(database, nil, "", "", 200, nil, "")
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	var costJobs []*db.Job
	for _, j := range jobs {
		if j.Cost != nil && *j.Cost > 0 {
			costJobs = append(costJobs, j)
		}
	}
	if len(costJobs) == 0 {
		fmt.Println("No jobs with recorded cost.")
		return nil
	}

	// Collect the unique launch IDs so we can look up instance data once.
	launchIDSet := make(map[int64]struct{})
	for _, j := range costJobs {
		if j.LaunchID != nil && *j.LaunchID > 0 {
			launchIDSet[*j.LaunchID] = struct{}{}
		}
	}
	launchIDs := make([]int64, 0, len(launchIDSet))
	for id := range launchIDSet {
		launchIDs = append(launchIDs, id)
	}

	launches, err := db.GetLaunchesByIDs(database, launchIDs)
	if err != nil {
		return fmt.Errorf("get launches: %w", err)
	}
	jobCounts, err := db.GetLaunchJobCounts(database)
	if err != nil {
		return fmt.Errorf("get job counts: %w", err)
	}

	for i, j := range costJobs {
		if i > 0 {
			fmt.Println("---")
		}
		jobCost := campaign.FormatCostCents(*j.Cost * 100)
		project := j.Project
		if project == "" {
			project = "—"
		}

		field("Job:", ids.FormatJobID(j.ID))
		field("Project:", project)
		field("Status:", j.Status)
		field("Job cost:", jobCost)

		if j.LaunchID != nil && *j.LaunchID > 0 {
			inst := launches[*j.LaunchID]
			field("Instance:", ids.FormatInstanceID(*j.LaunchID))
			if inst != nil {
				_, instCostStr := instanceCostBreakdown(inst)
				field("Instance cost:", instCostStr)

				n := jobCounts[*j.LaunchID]
				if n > 0 {
					field("Jobs on instance:", fmt.Sprintf("%d", n))
					if inst.LaunchedAt != nil && inst.CostPerHourCents > 0 {
						var end time.Time
						if inst.EndedAt != nil {
							end = time.Unix(*inst.EndedAt, 0)
						} else {
							end = time.Now()
						}
						instCents := end.Sub(time.Unix(*inst.LaunchedAt, 0)).Hours() * float64(inst.CostPerHourCents)
						overheadCents := (instCents - (*j.Cost * 100)) / float64(n)
						if overheadCents > 0 {
							field("Overhead/job:", campaign.FormatCostCents(overheadCents))
						}
					}
				}
			}
		}
	}
	return nil
}

func runCostCampaigns(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	reconcileBeforeDisplay(database, FastCloudSyncTimeout)

	campaigns, err := db.ListCampaigns(database)
	if err != nil {
		return fmt.Errorf("list campaigns: %w", err)
	}
	if len(campaigns) == 0 {
		fmt.Println("No campaigns.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "Campaign\tStatus\tInstances\tEst. cost\tActual cost\n")
	for _, c := range campaigns {
		instances, _ := db.GetCampaignInstances(database, c.ID)
		estCost := campaign.FormatEstimatedCostCents(c.EstimatedCostCents)
		actualCost := campaignActualCost(instances)
		fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\n",
			c.ID, c.Status, len(instances), estCost, actualCost)
	}
	w.Flush()
	return nil
}

// field prints a single "Label:   value" line with values aligned at column 19.
func field(label, value string) {
	fmt.Printf("%-19s%s\n", label, value)
}

// instanceCostBreakdown returns the formatted duration and actual cost for a
// single instance based on uptime × rate.
func instanceCostBreakdown(inst *db.Launch) (duration, actualCost string) {
	if inst.LaunchedAt == nil {
		return "—", "—"
	}
	var end time.Time
	if inst.EndedAt != nil {
		end = time.Unix(*inst.EndedAt, 0)
	} else {
		end = time.Now()
	}
	uptime := end.Sub(time.Unix(*inst.LaunchedAt, 0))
	hours := uptime.Hours()
	cents := hours * float64(inst.CostPerHourCents)

	h := int(uptime.Hours())
	m := int(uptime.Minutes()) % 60
	if h > 0 {
		duration = fmt.Sprintf("%dh%02dm", h, m)
	} else {
		duration = fmt.Sprintf("%dm", m)
	}
	actualCost = campaign.FormatCostCents(cents)
	return duration, actualCost
}
