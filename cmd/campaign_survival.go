package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
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

	type groupInfo struct {
		family   string
		bucket   bidding.PriceBucket
		total    int
		survived int
	}
	var groups []groupInfo
	for key, stats := range model.Groups {
		idx := strings.LastIndex(key, ":")
		if idx < 0 {
			continue
		}
		groups = append(groups, groupInfo{
			family:   key[:idx],
			bucket:   bidding.PriceBucket(key[idx+1:]),
			total:    stats.Total,
			survived: stats.Survived,
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].family != groups[j].family {
			return groups[i].family < groups[j].family
		}
		return groups[i].bucket < groups[j].bucket
	})

	const displayReliability = 0.9

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "GPU FAMILY\tBUCKET\tOBS\tSURVIVED\tPOSTERIOR\tSTATUS\n")
	for _, g := range groups {
		posterior := model.SurvivalProbability(g.family, g.bucket, displayReliability)
		status := "ok"
		if posterior < campaignSurvivalFloor {
			status = fmt.Sprintf("BELOW FLOOR (%.0f%%)", campaignSurvivalFloor*100)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%.0f%%\t%s\n",
			g.family, g.bucket, g.total, g.survived, posterior*100, status)
	}
	w.Flush()

	type machineInfo struct {
		id       string
		total    int
		survived int
		penalty  float64
	}
	var badMachines []machineInfo
	for id, stats := range model.MachineStats {
		penalty := model.MachinePenalty(id)
		if penalty < 1.0 {
			badMachines = append(badMachines, machineInfo{
				id: id, total: stats.Total, survived: stats.Survived, penalty: penalty,
			})
		}
	}
	if len(badMachines) > 0 {
		sort.Slice(badMachines, func(i, j int) bool { return badMachines[i].penalty < badMachines[j].penalty })
		fmt.Printf("\nPer-machine penalties (%d+ observations, below-average survival):\n", bidding.MinMachineObs)
		for _, m := range badMachines {
			fmt.Printf("  machine %s: %d/%d survived (%.0f%%), penalty %.2f\n",
				m.id, m.survived, m.total, 100*float64(m.survived)/float64(m.total), m.penalty)
		}
	}

	return nil
}
