package campaign

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/ids"
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
	}
	var rows []row
	var totalCost float64
	hasAny := false

	for i, go_ := range groupOffers {
		if go_.Offer == nil {
			continue
		}
		totalJobs := len(go_.Group.Jobs)
		selected, scale := selectionScale(i, totalJobs, selectedPerGroup)
		if selected == 0 {
			continue
		}
		hasAny = true

		estCost := float64(totalJobs) * go_.Offer.CostPerHour * scale
		totalCost += estCost

		rows = append(rows, row{
			gpu:  FormatResolvedGPU(go_.Group.GPUSpec(), go_.Offer.GPUName),
			jobs: PluralJobs(selected),
			mem:  fmt.Sprintf("%dGB", int(go_.Offer.GPUMemGB)),
			rate: fmt.Sprintf("$%.2f/hr", go_.Offer.CostPerHour),
			cost: fmt.Sprintf("~$%.2f", estCost),
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
		lines = append(lines, CostLine{Text: line})
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
	s := estimate.FormatDurationShort(d)
	if hasPrediction {
		return "~" + s
	}
	return "~" + s + " (est)"
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
		parts = append(parts, ids.FormatJobID(j.ID))
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
	Lines         []CostLine
	TimeColOffset int // character offset where the time column starts (for aligning sub-tables)
	TimeWidth     int // width of the time/duration column (for aligning detail rows)
	RateWidth     int // width of the rate column (for aligning detail rows)
}

// selectionScale computes the fraction of selected jobs and the count for a group.
// When selectedPerGroup is nil, all jobs are treated as selected.
func selectionScale(groupIdx int, totalJobs int, selectedPerGroup []int) (selected int, scale float64) {
	if selectedPerGroup == nil {
		return totalJobs, 1.0
	}
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
// Groups with 0 selected jobs are omitted.
func FormatCostTableSelected(estimates []CostEstimate, selectedPerGroup []int, minDurWidth, minRateWidth int) CostTable {
	type row struct {
		gpu, jobs, rate, dur, cost string
	}
	var rows []row
	var totalCost, totalLower, totalUpper float64
	hasAny := false

	for i, est := range estimates {
		if est.Offer.Offer == nil {
			continue
		}

		totalJobs := len(est.Group.Jobs)
		selected, scale := selectionScale(i, totalJobs, selectedPerGroup)
		if selected == 0 {
			continue
		}
		hasAny = true

		scaledTime := est.Breakdown.Total.Scale(scale)
		scaledCost := est.TotalCost * scale
		totalCost += scaledCost
		totalLower += scaledTime.Lower.Hours() * est.Offer.Offer.CostPerHour
		totalUpper += scaledTime.Upper.Hours() * est.Offer.Offer.CostPerHour

		rows = append(rows, row{
			gpu:  est.Offer.Offer.GPUName,
			jobs: PluralJobs(selected),
			rate: fmt.Sprintf("$%.2f/hr", est.Offer.Offer.CostPerHour),
			dur:  formatDurationWithBounds(scaledTime),
			cost: formatCostWithBounds(scaledCost, scaledTime, est.Offer.Offer.CostPerHour),
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
	wDur = max(wDur, minDurWidth)
	wRate = max(wRate, minRateWidth)

	// Columns: dur rate cost gpu jobs — time/cost first to align with summary table above
	var lines []CostLine
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %s  %-*s  %s", wDur, r.dur, wRate, r.rate, r.cost, wGPU, r.gpu, r.jobs)
		lines = append(lines, CostLine{Text: line})
	}

	var totalStr string
	if totalUpper-totalLower < 0.01 {
		totalStr = fmt.Sprintf("Total: ~$%.2f", totalCost)
	} else {
		totalStr = fmt.Sprintf("Total: ~$%.2f ($%.2f–$%.2f)", totalCost, totalLower, totalUpper)
	}
	totalLine := fmt.Sprintf("%-*s  %-*s  %s", wDur, "", wRate, "", totalStr)
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
	return e.FormatWithBounds()
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

// FormatStrategySummary returns a comparison table showing all strategies.
// Active row gets a "►" prefix; others are dimmed.
func FormatStrategySummary(rows []StrategySummaryRow) CostTable {
	type fmtRow struct {
		prefix, strategy, time, rate, cost, instances string
		dimmed                                        bool
	}

	var fRows []fmtRow
	for _, r := range rows {
		prefix := "  "
		dimmed := true
		if r.Active {
			if r.Disclosed {
				prefix = "▾ "
			} else {
				prefix = "▸ "
			}
			dimmed = false
		}
		if r.Loading {
			fRows = append(fRows, fmtRow{
				prefix:   prefix,
				strategy: r.Label,
				time:     "estimating...",
				dimmed:   dimmed,
			})
			continue
		}
		timeStr := formatDurationWithBounds(r.MaxTime)
		rateStr := fmt.Sprintf("$%.2f/hr", r.TotalRate)
		costStr := formatCostBounds(r.TotalCost, r.CostLower, r.CostUpper)

		instanceStr := fmt.Sprintf("%d instance", r.NumGPUs)
		if r.NumGPUs != 1 {
			instanceStr += "s"
		}

		fRows = append(fRows, fmtRow{
			prefix:    prefix,
			strategy:  r.Label,
			time:      timeStr,
			rate:      rateStr,
			cost:      costStr,
			instances: instanceStr,
			dimmed:    dimmed,
		})
	}

	var wStrat, wTime, wRate int
	for _, r := range fRows {
		if len(r.strategy) > wStrat {
			wStrat = len(r.strategy)
		}
		if len(r.time) > wTime {
			wTime = len(r.time)
		}
		if len(r.rate) > wRate {
			wRate = len(r.rate)
		}
	}

	timeColOffset := 2 + wStrat + 2 // prefix(2) + strategy + gap(2)
	var lines []CostLine
	for _, r := range fRows {
		line := fmt.Sprintf("%s%-*s  %-*s  %-*s  %s",
			r.prefix, wStrat, r.strategy, wTime, r.time, wRate, r.rate, r.cost)
		if r.instances != "" {
			line += "  " + r.instances
		}
		lines = append(lines, CostLine{Text: line, Dimmed: r.dimmed})
	}
	return CostTable{Lines: lines, TimeColOffset: timeColOffset, TimeWidth: wTime, RateWidth: wRate}
}

// formatCostBounds formats "$X.XX ($L–$U)" or just "$X.XX" if bounds are tight.
func formatCostBounds(mean, lower, upper float64) string {
	if mean == 0 {
		return "—"
	}
	if upper-lower < 0.01 || lower == mean {
		return fmt.Sprintf("~$%.2f", mean)
	}
	return fmt.Sprintf("~$%.2f ($%.2f–$%.2f)", mean, lower, upper)
}

// FormatParetoSparkline renders a small ASCII chart of cost (Y) vs time (X)
// with points for each strategy and horizontal error bars for time bounds.
// Returns nil if fewer than 2 data points. Lines containing the active
// strategy's point are not dimmed; all others are dimmed.
func FormatParetoSparkline(rows []StrategySummaryRow) *CostTable {
	// Collect plottable points
	type point struct {
		timeMean  float64 // hours
		timeLower float64
		timeUpper float64
		costMean  float64
		label     string
		active    bool
	}
	var pts []point
	for _, r := range rows {
		if r.Loading || r.MaxTime.Mean == 0 {
			continue
		}
		pts = append(pts, point{
			timeMean:  r.MaxTime.Mean.Hours(),
			timeLower: r.MaxTime.Lower.Hours(),
			timeUpper: r.MaxTime.Upper.Hours(),
			costMean:  r.TotalCost,
			label:     r.Label,
			active:    r.Active,
		})
	}
	if len(pts) < 2 {
		return nil
	}

	// Find axis ranges — include error bar extents and mean values
	minT, maxT := pts[0].timeLower, pts[0].timeUpper
	minC, maxC := pts[0].costMean, pts[0].costMean
	for _, p := range pts {
		if p.timeLower < minT {
			minT = p.timeLower
		}
		if p.timeMean < minT {
			minT = p.timeMean
		}
		if p.timeUpper > maxT {
			maxT = p.timeUpper
		}
		if p.timeMean > maxT {
			maxT = p.timeMean
		}
		if p.costMean < minC {
			minC = p.costMean
		}
		if p.costMean > maxC {
			maxC = p.costMean
		}
	}

	// Add padding so points don't land on edges
	rangeT := maxT - minT
	rangeC := maxC - minC
	if rangeT < 0.01 {
		rangeT = 1.0
		minT -= 0.5
		maxT += 0.5
	} else {
		pad := rangeT * 0.15
		minT -= pad
		maxT += pad
		rangeT = maxT - minT
	}
	if rangeC < 0.01 {
		rangeC = 1.0
		minC -= 0.5
		maxC += 0.5
	} else {
		pad := rangeC * 0.15
		minC -= pad
		maxC += pad
		rangeC = maxC - minC
	}
	if minT < 0 {
		minT = 0
	}
	if minC < 0 {
		minC = 0
	}

	// Chart dimensions
	const chartRows = 3
	const chartCols = 28
	const yLabelW = 8 // e.g. "  $12.34 "

	// Helper to map a time value to a column
	timeToCol := func(t float64) int {
		col := int(math.Round(float64(chartCols-1) * (t - minT) / rangeT))
		if col < 0 {
			col = 0
		}
		if col >= chartCols {
			col = chartCols - 1
		}
		return col
	}

	// Map points to grid positions
	type placed struct {
		col      int
		row      int
		colLower int // error bar left
		colUpper int // error bar right
		label    string
		active   bool
	}
	var placements []placed
	rowCosts := make(map[int][]float64)
	for _, p := range pts {
		col := timeToCol(p.timeMean)
		row := chartRows - 1 - int(math.Round(float64(chartRows-1)*(p.costMean-minC)/rangeC))
		if row < 0 {
			row = 0
		}
		if row >= chartRows {
			row = chartRows - 1
		}
		colLower := timeToCol(p.timeLower)
		colUpper := timeToCol(p.timeUpper)
		placements = append(placements, placed{col: col, row: row, colLower: colLower, colUpper: colUpper, label: p.label, active: p.active})
		rowCosts[row] = append(rowCosts[row], p.costMean)
	}

	// Build grid (rows × cols of runes)
	grid := make([][]rune, chartRows)
	for r := range grid {
		grid[r] = make([]rune, chartCols)
		for c := range grid[r] {
			grid[r][c] = ' '
		}
	}

	// Draw error bars first (so point markers overwrite them)
	for _, p := range placements {
		if p.colUpper > p.colLower {
			// Left cap
			if grid[p.row][p.colLower] == ' ' {
				grid[p.row][p.colLower] = '├'
			}
			// Horizontal bar
			for c := p.colLower + 1; c < p.colUpper; c++ {
				if grid[p.row][c] == ' ' {
					grid[p.row][c] = '─'
				}
			}
			// Right cap
			if grid[p.row][p.colUpper] == ' ' {
				grid[p.row][p.colUpper] = '┤'
			}
		}
	}

	// Draw active-strategy points last so they overwrite inactive ones on collision
	type rowLabel struct {
		label string
	}
	rowLabels := make(map[int][]rowLabel)
	activeRows := make(map[int]bool)

	// Inactive points first, then active (so active overwrites on collision)
	for pass := 0; pass < 2; pass++ {
		for _, p := range placements {
			if (pass == 0 && p.active) || (pass == 1 && !p.active) {
				continue
			}
			grid[p.row][p.col] = '●'
			rowLabels[p.row] = append(rowLabels[p.row], rowLabel{label: p.label})
			if p.active {
				activeRows[p.row] = true
			}
		}
	}

	// Y-axis labels: use actual cost values for rows with points, interpolated for empty rows
	var yLabels [chartRows]string
	for r := 0; r < chartRows; r++ {
		costs := rowCosts[r]
		if len(costs) > 0 && uniqueCostCount(costs) == 1 {
			yLabels[r] = formatDollarLabel(costs[0])
		} else {
			// Empty row: round the interpolated value so its label doesn't
			// collide with labels on adjacent occupied rows.
			frac := float64(r) / float64(chartRows-1)
			v := maxC - frac*(maxC-minC)

			// Collect formatted labels already assigned to other rows
			usedLabels := make(map[string]bool)
			for rr := 0; rr < chartRows; rr++ {
				if rr != r && yLabels[rr] != "" {
					usedLabels[yLabels[rr]] = true
				}
				if rr != r {
					if c := rowCosts[rr]; len(c) > 0 && uniqueCostCount(c) == 1 {
						usedLabels[formatDollarLabel(c[0])] = true
					}
				}
			}
			yLabels[r] = formatDollarLabel(roundBetweenAvoidLabels(v, minC, maxC, usedLabels))
		}
	}

	// Build CostTable lines — active-point rows are not dimmed (rendered in highlight color)
	var lines []CostLine
	for r := 0; r < chartRows; r++ {
		var b strings.Builder
		b.WriteString(fmt.Sprintf("%*s┤", yLabelW, yLabels[r]))
		b.WriteString(string(grid[r][:]))
		for _, rl := range rowLabels[r] {
			b.WriteString(" " + rl.label)
		}
		lines = append(lines, CostLine{Text: b.String(), Dimmed: !activeRows[r]})
	}

	// X-axis line
	xAxis := strings.Repeat(" ", yLabelW) + "└" + strings.Repeat("─", chartCols)
	lines = append(lines, CostLine{Text: xAxis, Dimmed: true})

	// X-axis labels
	leftLabel := estimate.FormatDurationShort(time.Duration(minT * float64(time.Hour)))
	midLabel := estimate.FormatDurationShort(time.Duration((minT + maxT) / 2 * float64(time.Hour)))
	rightLabel := estimate.FormatDurationShort(time.Duration(maxT * float64(time.Hour)))

	xLine := make([]byte, yLabelW+1+chartCols)
	for i := range xLine {
		xLine[i] = ' '
	}
	leftPos := yLabelW + 1
	copy(xLine[leftPos:], []byte(leftLabel))
	rightPos := yLabelW + 1 + chartCols - len(rightLabel)
	if rightPos < leftPos+len(leftLabel)+1 {
		rightPos = leftPos + len(leftLabel) + 1
	}
	copy(xLine[rightPos:], []byte(rightLabel))
	midPos := yLabelW + 1 + chartCols/2 - len(midLabel)/2
	if midPos > leftPos+len(leftLabel)+1 && midPos+len(midLabel) < rightPos-1 {
		copy(xLine[midPos:], []byte(midLabel))
	}
	lines = append(lines, CostLine{Text: string(xLine), Dimmed: true})

	return &CostTable{Lines: lines}
}

// uniqueCostCount returns the number of distinct values in costs.
func uniqueCostCount(costs []float64) int {
	seen := make(map[float64]bool)
	for _, c := range costs {
		seen[c] = true
	}
	return len(seen)
}

// roundBetweenAvoidLabels rounds v to a clean value that stays strictly between
// lo and hi, and whose formatted label doesn't collide with usedLabels.
func roundBetweenAvoidLabels(v, lo, hi float64, usedLabels map[string]bool) float64 {
	for _, unit := range []float64{1.0, 0.5, 0.25, 0.1, 0.05} {
		rounded := math.Round(v/unit) * unit
		if rounded > lo && rounded < hi && !usedLabels[formatDollarLabel(rounded)] {
			return rounded
		}
	}
	return v
}

// formatDollarLabel formats a dollar amount for sparkline Y-axis.
func formatDollarLabel(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("$%.0f", v)
	}
	if v >= 10 {
		return fmt.Sprintf("$%.1f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}
