package cmd

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var campaignSurvivalFloor float64

var campaignSurvivalCmd = &cobra.Command{
	Use:   "survival",
	Short: "Show survival model used for offer selection",
	Args:  cobra.NoArgs,
	RunE:  runCampaignSurvival,
}

func init() {
	campaignCmd.AddCommand(campaignSurvivalCmd)
	campaignSurvivalCmd.Flags().Float64Var(&campaignSurvivalFloor, "floor", 0.4, "Highlight entries below this survival probability")
}

func runCampaignSurvival(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	model := buildSurvivalModel(database)
	if model == nil {
		fmt.Println("No terminal cloud instances with survival data found.")
		return nil
	}

	groups := model.IterateGroups()
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Provider != groups[j].Provider {
			return groups[i].Provider < groups[j].Provider
		}
		if groups[i].Family != groups[j].Family {
			return groups[i].Family < groups[j].Family
		}
		return groups[i].Bucket < groups[j].Bucket
	})

	const displayReliability = 0.9

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "PROVIDER\tGPU FAMILY\tBUCKET\tOBS\tSURVIVED\tPOSTERIOR\tSTATUS\n")
	for _, g := range groups {
		posterior := model.SurvivalProbability(g.Provider, g.Family, g.Bucket, displayReliability)
		status := "ok"
		if posterior < campaignSurvivalFloor {
			status = fmt.Sprintf("BELOW FLOOR (%.0f%%)", campaignSurvivalFloor*100)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%.0f%%\t%s\n",
			g.Provider, g.Family, g.Bucket, g.Total, g.Survived, posterior*100, status)
	}
	w.Flush()

	type machineInfo struct {
		stat    bidding.MachineStat
		penalty float64
	}
	var badMachines []machineInfo
	for _, ms := range model.IterateMachines() {
		penalty := model.MachinePenalty(ms.Provider, ms.ID)
		if penalty < 1.0 {
			badMachines = append(badMachines, machineInfo{stat: ms, penalty: penalty})
		}
	}
	if len(badMachines) > 0 {
		sort.Slice(badMachines, func(i, j int) bool { return badMachines[i].penalty < badMachines[j].penalty })
		fmt.Printf("\nPer-machine penalties (Beta posterior vs provider global, prior strength %.0f):\n", bidding.MachinePriorStrength)
		for _, m := range badMachines {
			fmt.Printf("  %s machine %s: %d/%d survived (%.0f%%), penalty %.2f\n",
				m.stat.Provider, m.stat.ID, m.stat.Survived, m.stat.Total,
				100*float64(m.stat.Survived)/float64(m.stat.Total), m.penalty)
		}
	}

	return nil
}
