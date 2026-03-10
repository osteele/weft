package cmd

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var campaignStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show aggregate cloud instance statistics",
	Args:  cobra.NoArgs,
	RunE:  runCampaignStats,
}

type instanceStats struct {
	TerminationReason string
	CostPerHourCents  int
	ResolvedGPUName   sql.NullString
	LaunchedAt        sql.NullString
	EndedAt           sql.NullString
	ActualSpendCents  sql.NullInt64
}

type gpuFamilyStats struct {
	Total     int
	Survived  int
	Preempted int
	Wasted    float64 // dollars wasted on preempted instances
	TotalRate float64 // sum of cost_per_hour for avg calculation
}

func runCampaignStats(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	reconcileBeforeDisplay(database)

	rows, err := database.Query(`
		SELECT termination_reason, cost_per_hour_cents, resolved_gpu_name,
		       launched_at, ended_at, actual_spend_cents
		FROM cloud_instances
		WHERE status IN ('completed', 'failed', 'cancelled')
		  AND termination_reason IS NOT NULL
		  AND termination_reason != ''
		ORDER BY id
	`)
	if err != nil {
		return fmt.Errorf("query instances: %w", err)
	}
	defer rows.Close()

	var instances []instanceStats
	for rows.Next() {
		var s instanceStats
		if err := rows.Scan(&s.TerminationReason, &s.CostPerHourCents, &s.ResolvedGPUName,
			&s.LaunchedAt, &s.EndedAt, &s.ActualSpendCents); err != nil {
			return fmt.Errorf("scan instance: %w", err)
		}
		instances = append(instances, s)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate instances: %w", err)
	}

	if len(instances) == 0 {
		fmt.Println("No terminal cloud instances found.")
		return nil
	}

	// Count by termination reason
	reasonCounts := make(map[string]int)
	var totalSpend, wastedSpend float64
	gpuFamilies := make(map[string]*gpuFamilyStats)

	for _, inst := range instances {
		reasonCounts[inst.TerminationReason]++

		spend := float64(inst.ActualSpendCents.Int64) / 100.0
		totalSpend += spend

		survived := inst.TerminationReason == db.TerminationReasonCompleted || inst.TerminationReason == db.TerminationReasonJobFailure
		preempted := inst.TerminationReason == db.TerminationReasonPreempted
		if preempted {
			wastedSpend += spend
		}

		family := bidding.NormalizeGPUFamily(inst.ResolvedGPUName.String)
		if family == "" {
			family = "unknown"
		}
		gs, ok := gpuFamilies[family]
		if !ok {
			gs = &gpuFamilyStats{}
			gpuFamilies[family] = gs
		}
		gs.Total++
		if survived {
			gs.Survived++
		}
		if preempted {
			gs.Preempted++
			gs.Wasted += spend
		}
		gs.TotalRate += float64(inst.CostPerHourCents) / 100.0
	}

	// Print overall summary
	total := len(instances)
	fmt.Printf("Cloud Instance Statistics (%d instances)\n\n", total)
	fmt.Println("Overall:")

	// Print reasons in a stable order
	reasonOrder := []struct {
		key   string
		label string
	}{
		{db.TerminationReasonCompleted, "Completed"},
		{db.TerminationReasonPreempted, "Preempted"},
		{db.TerminationReasonInfraFailure, "Infra failure"},
		{db.TerminationReasonJobFailure, "Job failure"},
		{db.TerminationReasonCancelled, "Cancelled"},
	}
	for _, r := range reasonOrder {
		if count, ok := reasonCounts[r.key]; ok {
			pct := 100.0 * float64(count) / float64(total)
			fmt.Printf("  %-16s %3d (%5.1f%%)\n", r.label+":", count, pct)
			delete(reasonCounts, r.key)
		}
	}
	// Print any remaining reasons not in the known list
	remaining := make([]string, 0, len(reasonCounts))
	for k := range reasonCounts {
		remaining = append(remaining, k)
	}
	sort.Strings(remaining)
	for _, k := range remaining {
		count := reasonCounts[k]
		pct := 100.0 * float64(count) / float64(total)
		label := strings.ReplaceAll(k, "_", " ")
		label = strings.ToUpper(label[:1]) + label[1:]
		fmt.Printf("  %-16s %3d (%5.1f%%)\n", label+":", count, pct)
	}

	fmt.Println()
	fmt.Printf("  Total spend:          $%.2f\n", totalSpend)
	if wastedSpend > 0 {
		wastedPct := 100.0 * wastedSpend / totalSpend
		fmt.Printf("  Wasted (preempted):   $%.2f (%.1f%%)\n", wastedSpend, wastedPct)
	}

	// Print GPU family breakdown
	if len(gpuFamilies) > 0 {
		fmt.Println("\nBy GPU family:")
		fmt.Printf("  %-16s %9s  %8s  %9s  %9s  %8s  %6s\n",
			"GPU", "INSTANCES", "SURVIVED", "PREEMPTED", "SURVIVAL%", "AVG $/HR", "WASTED")

		families := make([]string, 0, len(gpuFamilies))
		for k := range gpuFamilies {
			families = append(families, k)
		}
		sort.Strings(families)

		for _, fam := range families {
			gs := gpuFamilies[fam]
			survPct := 100.0 * float64(gs.Survived) / float64(gs.Total)
			avgRate := gs.TotalRate / float64(gs.Total)
			fmt.Printf("  %-16s %9d  %8d  %9d  %8.1f%%  %7s  %6s\n",
				fam, gs.Total, gs.Survived, gs.Preempted, survPct,
				fmt.Sprintf("$%.2f", avgRate),
				fmt.Sprintf("$%.2f", gs.Wasted))
		}
	}

	return nil
}
