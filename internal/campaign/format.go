package campaign

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/vastai"
)

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

		b.WriteString(fmt.Sprintf("%-10s %d jobs  %s %dGB   $%.2f/hr  ~$%.2f\n",
			go_.Group.GPUSpec(),
			jobs,
			go_.Offer.GPUName,
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
