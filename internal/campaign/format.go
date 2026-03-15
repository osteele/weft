package campaign

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

// FormatResolvedGPU formats the GPU constraint and resolved name.
// When the constraint matches the resolved name (normalized), shows just the resolved name.
// Otherwise shows "CONSTRAINT → RESOLVED".
func FormatResolvedGPU(gpuSpec string, resolvedName string) string {
	// Normalize for comparison: strip non-alphanumeric, lowercase
	norm := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	if norm(gpuSpec) == norm(resolvedName) {
		return resolvedName
	}
	return gpuSpec + " → " + resolvedName
}

// FormatJobLine returns a display line for a job in the launch selector.
// Format: "  88  EXP-030: Idle power investigation (sole-tenant A100)"
func FormatJobLine(job *db.Job) string {
	desc := job.Description
	if desc == "" {
		desc = TruncateCommand(job.Command, 60)
	}
	project := JobProjectLabel(job)
	if project == "" {
		return fmt.Sprintf("%4d  %s", job.ID, desc)
	}
	return fmt.Sprintf("%4d  %-18s  %s", job.ID, project, desc)
}

// JobProjectLabel returns the display label for a job's project in summary
// views, preferring the stored project and falling back to the directory tail.
func JobProjectLabel(job *db.Job) string {
	if job == nil {
		return ""
	}
	if project := strings.TrimSpace(job.Project); project != "" {
		return project
	}
	return job.DirectoryTailDisplay()
}

// FormatCostTable returns a rough cost estimate table (1hr/job) for the given
// group offers, with selection awareness. Dimmed lines for groups with 0 selected.
func FormatCostTable(groupOffers []GroupOffer, selectedPerGroup []int) CostTable {
	type row struct {
		gpu, jobs, mem, rate, cost string
		dimmed                     bool
	}
	var rows []row
	var totalCost float64
	hasAny := false

	for i, go_ := range groupOffers {
		if go_.Offer == nil {
			continue
		}
		hasAny = true
		totalJobs := len(go_.Group.Jobs)
		selected, scale := selectionScale(i, totalJobs, selectedPerGroup)

		estCost := float64(totalJobs) * go_.Offer.CostPerHour * scale
		totalCost += estCost

		rows = append(rows, row{
			gpu:    FormatResolvedGPU(go_.Group.GPUSpec(), go_.Offer.GPUName),
			jobs:   PluralJobs(selected),
			mem:    fmt.Sprintf("%dGB", int(go_.Offer.GPUMemGB)),
			rate:   fmt.Sprintf("$%.2f/hr", go_.Offer.CostPerHour),
			cost:   fmt.Sprintf("~$%.2f", estCost),
			dimmed: selected == 0,
		})
	}

	if !hasAny {
		return CostTable{Lines: []CostLine{{Text: "  No offers found for any group."}}}
	}

	var wGPU, wJobs, wMem, wRate int
	for _, r := range rows {
		if len(r.gpu) > wGPU {
			wGPU = len(r.gpu)
		}
		if len(r.jobs) > wJobs {
			wJobs = len(r.jobs)
		}
		if len(r.mem) > wMem {
			wMem = len(r.mem)
		}
		if len(r.rate) > wRate {
			wRate = len(r.rate)
		}
	}

	var lines []CostLine
	fmtStr := fmt.Sprintf("%%-%ds  %%%ds  %%%ds  %%%ds  %%s", wGPU, wJobs, wMem, wRate)
	for _, r := range rows {
		line := fmt.Sprintf(fmtStr, r.gpu, r.jobs, r.mem, r.rate, r.cost)
		lines = append(lines, CostLine{Text: line, Dimmed: r.dimmed})
	}

	totalLine := fmt.Sprintf(fmtStr, "", "", "", "", fmt.Sprintf("Total: ~$%.2f", totalCost))
	lines = append(lines, CostLine{Text: totalLine})
	return CostTable{Lines: lines}
}

// FormatCostTableWithEstimates returns a cost table using predictor-based duration estimates.
func FormatCostTableWithEstimates(estimates []CostEstimate) string {
	var b strings.Builder
	var totalCost float64
	hasAny := false

	for _, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}
		hasAny = true
		totalCost += est.TotalCost

		gpuLabel := FormatResolvedGPU(est.Group.GPUSpec(), est.Offer.Offer.GPUName)
		durStr := FormatEstDuration(est.TotalTime, len(est.JobDurations) > 0)
		costStr := fmt.Sprintf("~$%.2f", est.TotalCost)
		if est.SurvivalProb > 0 && est.RiskAdjustedCost > est.TotalCost*1.1 {
			costStr = fmt.Sprintf("~$%.2f (risk: ~$%.2f)", est.TotalCost, est.RiskAdjustedCost)
		}
		survStr := ""
		if est.SurvivalProb > 0 {
			survStr = fmt.Sprintf("  %.0f%% surv", est.SurvivalProb*100)
		}
		b.WriteString(fmt.Sprintf("%-18s %-7s  %dGB   $%.2f/hr  %s  %s%s\n",
			gpuLabel,
			PluralJobs(len(est.Group.Jobs)),
			int(est.Offer.Offer.GPUMemGB),
			est.Offer.Offer.CostPerHour,
			durStr,
			costStr,
			survStr,
		))
	}

	if !hasAny {
		return "  No offers found for any group.\n"
	}

	b.WriteString(fmt.Sprintf("%50s Total: ~$%.2f\n", "", totalCost))
	return b.String()
}

// FormatEstDuration formats a duration with an indicator of whether it's predicted.
func FormatEstDuration(d time.Duration, hasPrediction bool) string {
	s := formatDurationShort(d)
	if hasPrediction {
		return "~" + s
	}
	return "~" + s + " (est)"
}

// formatDurationShort formats a duration as a compact string like "2h15".
func formatDurationShort(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%02d", h, m)
}

// FormatSSHCommand returns the SSH command string for a cloud instance.
func FormatSSHCommand(inst *cloud.Instance) string {
	return fmt.Sprintf("ssh -p %d -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null root@%s",
		inst.SSHPort, inst.SSHHost)
}

// FormatJobIDs returns a truncated list of job IDs for display.
// Shows at most maxShow IDs, then "+N more".
func FormatJobIDs(jobs []*db.Job, maxShow int) string {
	if len(jobs) == 0 {
		return ""
	}
	var parts []string
	for i, j := range jobs {
		if i >= maxShow && len(jobs) > maxShow {
			parts = append(parts, fmt.Sprintf("…+%d", len(jobs)-maxShow))
			break
		}
		parts = append(parts, fmt.Sprintf("%d", j.ID))
	}
	return strings.Join(parts, ",")
}

// TotalEstimatedCost returns the total estimated cost across all group offers.
// Estimates 1 hour per job as a rough approximation.
func TotalEstimatedCost(groupOffers []GroupOffer) float64 {
	var total float64
	for _, go_ := range groupOffers {
		if go_.Offer == nil {
			continue
		}
		total += float64(len(go_.Group.Jobs)) * go_.Offer.CostPerHour
	}
	return total
}

// CostLine is a single line in the cost table with styling metadata.
type CostLine struct {
	Text   string
	Dimmed bool
}

// CostTable is the result of FormatCostTableSelected.
type CostTable struct {
	Lines []CostLine
}

// selectionScale computes the fraction of selected jobs and the count for a group.
func selectionScale(groupIdx int, totalJobs int, selectedPerGroup []int) (selected int, scale float64) {
	if groupIdx < len(selectedPerGroup) {
		selected = selectedPerGroup[groupIdx]
	}
	if totalJobs > 0 {
		scale = float64(selected) / float64(totalJobs)
	}
	return selected, scale
}

// FormatCostTableSelected returns a selection-aware cost estimate table.
// Each group line shows "selected/total jobs" and scales cost/time proportionally.
// Lines for groups with 0 selected are marked as Dimmed.
func FormatCostTableSelected(estimates []CostEstimate, selectedPerGroup []int) CostTable {
	type row struct {
		gpu, jobs, rate, dur, cost string
		dimmed                     bool
	}
	var rows []row
	var totalCost, totalLower, totalUpper float64
	hasAny := false

	for i, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}
		hasAny = true

		totalJobs := len(est.Group.Jobs)
		selected, scale := selectionScale(i, totalJobs, selectedPerGroup)

		scaledTime := est.Breakdown.Total.Scale(scale)
		scaledCost := est.TotalCost * scale
		totalCost += scaledCost
		totalLower += scaledTime.Lower.Hours() * est.Offer.Offer.CostPerHour
		totalUpper += scaledTime.Upper.Hours() * est.Offer.Offer.CostPerHour

		rows = append(rows, row{
			gpu:    FormatResolvedGPU(est.Group.GPUSpec(), est.Offer.Offer.GPUName),
			jobs:   PluralJobs(selected),
			rate:   fmt.Sprintf("$%.2f/hr", est.Offer.Offer.CostPerHour),
			dur:    formatDurationWithBounds(scaledTime),
			cost:   formatCostWithBounds(scaledCost, scaledTime, est.Offer.Offer.CostPerHour),
			dimmed: scale == 0,
		})
	}

	if !hasAny {
		return CostTable{Lines: []CostLine{{Text: "  No offers found for any group."}}}
	}

	var wGPU, wJobs, wRate, wDur int
	for _, r := range rows {
		if len(r.gpu) > wGPU {
			wGPU = len(r.gpu)
		}
		if len(r.jobs) > wJobs {
			wJobs = len(r.jobs)
		}
		if len(r.rate) > wRate {
			wRate = len(r.rate)
		}
		if len(r.dur) > wDur {
			wDur = len(r.dur)
		}
	}

	var lines []CostLine
	fmtStr := fmt.Sprintf("%%-%ds  %%%ds  %%%ds  %%%ds  %%s", wGPU, wJobs, wRate, wDur)
	for _, r := range rows {
		line := fmt.Sprintf(fmtStr, r.gpu, r.jobs, r.rate, r.dur, r.cost)
		lines = append(lines, CostLine{Text: line, Dimmed: r.dimmed})
	}

	var totalStr string
	if totalUpper-totalLower < 0.01 {
		totalStr = fmt.Sprintf("Total: ~$%.2f", totalCost)
	} else {
		totalStr = fmt.Sprintf("Total: ~$%.2f ($%.2f–$%.2f)", totalCost, totalLower, totalUpper)
	}
	totalLine := fmt.Sprintf(fmtStr, "", "", "", "", totalStr)
	lines = append(lines, CostLine{Text: totalLine})
	return CostTable{Lines: lines}
}

// FormatCostBreakdown returns a detailed cost breakdown per group, showing
// startup, provisioning, and runtime phases with time/cost bounds.
func FormatCostBreakdown(estimates []CostEstimate, selectedPerGroup []int) string {
	var b strings.Builder
	b.WriteString("  Cost Breakdown\n\n")

	for i, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}

		totalJobs := len(est.Group.Jobs)
		selected, scale := selectionScale(i, totalJobs, selectedPerGroup)

		gpuLabel := FormatResolvedGPU(est.Group.GPUSpec(), est.Offer.Offer.GPUName)
		b.WriteString(fmt.Sprintf("  %s ($%.2f/hr, %s selected)\n",
			gpuLabel, est.Offer.Offer.CostPerHour, PluralJobs(selected)))

		// Startup phase
		b.WriteString(fmt.Sprintf("    Instance startup   %s\n",
			formatDurationWithBounds(est.Breakdown.Startup)))

		// Provision phase
		provLine := fmt.Sprintf("    Provisioning       %s",
			formatDurationWithBounds(est.Breakdown.Provision))
		if est.DownloadBytes > 0 {
			provLine += fmt.Sprintf("     %s model download", formatBytes(est.DownloadBytes))
		}
		b.WriteString(provLine + "\n")

		// Run phase (scaled by selection)
		scaledRun := est.Breakdown.Run.Scale(scale)
		runLine := fmt.Sprintf("    Job runtime        %s",
			formatDurationWithBounds(scaledRun))
		runLine += fmt.Sprintf("     %s, sequential", PluralJobs(selected))
		b.WriteString(runLine + "\n")

		// Total
		scaledTotal := est.Breakdown.Startup.Add(est.Breakdown.Provision).Add(scaledRun)
		totalCost := scaledTotal.Mean.Hours() * est.Offer.Offer.CostPerHour
		costStr := formatCostWithBounds(totalCost, scaledTotal, est.Offer.Offer.CostPerHour)
		b.WriteString(fmt.Sprintf("    Total              %s          %s\n",
			formatDurationWithBounds(scaledTotal), costStr))

		b.WriteString("\n")
	}

	b.WriteString("  press d to return\n")
	return b.String()
}

// formatDurationWithBounds formats a duration estimate as "~2h15 (1h–4h)".
func formatDurationWithBounds(e estimate.Estimate) string {
	if e.Mean == 0 {
		return "—"
	}
	mean := formatDurationShort(e.Mean)
	if e.Lower == e.Upper || e.Lower == e.Mean {
		return "~" + mean
	}
	lower := formatDurationShort(e.Lower)
	upper := formatDurationShort(e.Upper)
	return fmt.Sprintf("~%s (%s–%s)", mean, lower, upper)
}

// formatCostWithBounds formats a cost as "~$3.38 ($1.00–$5.50)".
func formatCostWithBounds(meanCost float64, timeEst estimate.Estimate, costPerHour float64) string {
	if meanCost == 0 {
		return "—"
	}
	lowerCost := timeEst.Lower.Hours() * costPerHour
	upperCost := timeEst.Upper.Hours() * costPerHour
	if upperCost-lowerCost < 0.01 || lowerCost == meanCost {
		return fmt.Sprintf("~$%.2f", meanCost)
	}
	return fmt.Sprintf("~$%.2f ($%.2f–$%.2f)", meanCost, lowerCost, upperCost)
}

// formatBytes formats bytes as a human-readable string (e.g., "12 GB").
func formatBytes(bytes int64) string {
	const gb = 1024 * 1024 * 1024
	const mb = 1024 * 1024
	if bytes >= gb {
		return fmt.Sprintf("%d GB", bytes/gb)
	}
	return fmt.Sprintf("%d MB", bytes/mb)
}

// PluralJobs returns "1 job" or "N jobs".
func PluralJobs(n int) string {
	if n == 1 {
		return "1 job"
	}
	return fmt.Sprintf("%d jobs", n)
}

// FormatCostCents formats a cost in cents as "$X.XX", or returns "—" if zero.
func FormatCostCents(cents float64) string {
	if cents == 0 {
		return "—"
	}
	return fmt.Sprintf("$%.2f", cents/100)
}

// FormatEstimatedCostCents formats an estimated cost stored as integer cents.
func FormatEstimatedCostCents(cents int) string {
	if cents <= 0 {
		return "—"
	}
	return fmt.Sprintf("$%.2f", float64(cents)/100)
}

// TruncateCommand truncates a command string to max characters with ellipsis.
func TruncateCommand(cmd string, max int) string {
	if len(cmd) <= max {
		return cmd
	}
	return cmd[:max-1] + "…"
}
