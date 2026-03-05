package campaign

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/vastai"
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
	return fmt.Sprintf("%4d  %s", job.ID, desc)
}

// FormatCostTable returns a cost estimate table for the given group offers.
// Only includes groups where an offer was found.
func FormatCostTable(groupOffers []GroupOffer) string {
	var b strings.Builder
	var totalCost float64
	hasAny := false

	for _, go_ := range groupOffers {
		if go_.Offer == nil {
			continue
		}
		hasAny = true
		jobs := len(go_.Group.Jobs)
		// Rough estimate: 1 hour per job
		estCost := float64(jobs) * go_.Offer.CostPerHour
		totalCost += estCost

		gpuLabel := FormatResolvedGPU(go_.Group.GPUSpec(), go_.Offer.GPUName)
		b.WriteString(fmt.Sprintf("%-18s %d jobs  %dGB   $%.2f/hr  ~$%.2f\n",
			gpuLabel,
			jobs,
			int(go_.Offer.GPUMemGB),
			go_.Offer.CostPerHour,
			estCost,
		))
	}

	if !hasAny {
		return "  No offers found for any group.\n"
	}

	b.WriteString(fmt.Sprintf("%40s Total: ~$%.2f\n", "", totalCost))
	return b.String()
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
		durStr := FormatEstDuration(est.TotalTime, est.HasPrediction)
		b.WriteString(fmt.Sprintf("%-18s %d jobs  %dGB   $%.2f/hr  %s  ~$%.2f\n",
			gpuLabel,
			len(est.Group.Jobs),
			int(est.Offer.Offer.GPUMemGB),
			est.Offer.Offer.CostPerHour,
			durStr,
			est.TotalCost,
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

// formatDurationShort formats a duration as a compact string like "2h 15m".
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
	return fmt.Sprintf("%dh %dm", h, m)
}

// FormatSSHCommand returns the SSH command string for a Vast.ai instance.
func FormatSSHCommand(inst *vastai.Instance) string {
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

// TruncateCommand truncates a command string to max characters with ellipsis.
func TruncateCommand(cmd string, max int) string {
	if len(cmd) <= max {
		return cmd
	}
	return cmd[:max-1] + "…"
}
