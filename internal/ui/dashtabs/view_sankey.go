package dashtabs

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
)

type sankeyView struct{}

func newSankeyView() *sankeyView                       { return &sankeyView{} }
func (v *sankeyView) Title() string                    { return "Flow" }
func (v *sankeyView) ShortKey() string                 { return "8" }
func (v *sankeyView) Init() tea.Cmd                    { return nil }
func (v *sankeyView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

// Render shows job flow merged by destination — each instance appears once
// with running + queued + unplaced counts shown inline. Reads as a single
// left-to-right Sankey: destination · counts → provider.
//
//	rental wi3300  · 1r + 5q   ──▶ vastai · $0.93/hr
//	(unplaced)     · 1u
//
// Merging by destination (rather than splitting by source) matches the
// brainstorm mockup and removes the redundant double-listing of an instance
// that's both running one job and queued for several more.
func (v *sankeyView) Render(width, height int, snap Snapshot, _ bool) string {
	flow := buildFlow(snap)
	if flow.totalJobs == 0 && len(flow.providers) == 0 {
		return dimStyle.Render("Flow — no jobs to route.")
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("Flow — destination · jobs (Nr/Nq/Nu) → provider"))
	b.WriteString("\n\n")

	// Auto-size the destination label column to the longest label, plus
	// breathing room before the connector. Avoids the "valley" that comes
	// from always reserving a worst-case width.
	destColW := 0
	for dest := range flow.byDest {
		if w := len(dest); w > destColW {
			destColW = w
		}
	}
	destColW += 2

	// Sort destinations: rental first, on-prem next, unplaced last.
	// Within a kind, by total job count descending.
	dests := make([]string, 0, len(flow.byDest))
	for d := range flow.byDest {
		dests = append(dests, d)
	}
	sort.Slice(dests, func(i, j int) bool {
		a, c := dests[i], dests[j]
		da, dc := flow.byDest[a], flow.byDest[c]
		ka, kc := destKind(a), destKind(c)
		if ka != kc {
			return ka < kc
		}
		ta := da.Running + da.Queued + da.Unplaced
		tc := dc.Running + dc.Queued + dc.Unplaced
		if ta != tc {
			return ta > tc
		}
		return a < c
	})

	for _, dest := range dests {
		jc := flow.byDest[dest]
		breakdown := jobCountsLabel(jc)
		line := fmt.Sprintf("    %s %s %s %s",
			dimStyle.Render("└─▶"),
			accentStyle.Render(padRight(dest, destColW)),
			dimStyle.Render("·"),
			breakdown,
		)
		if provLabel := providerLabelForDest(dest, snap); provLabel != "" {
			line += "  " + dimStyle.Render("──▶") + "  " + provLabel
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	_ = width
	_ = height
	return b.String()
}

// destKind orders rental → on-prem → unplaced when sorting destinations.
func destKind(dest string) int {
	switch {
	case strings.HasPrefix(dest, "rental "):
		return 0
	case strings.HasPrefix(dest, "on-prem "):
		return 1
	default:
		return 2
	}
}

// jobCountsLabel formats running/queued/unplaced counts as "Nr + Nq + Nu",
// omitting zero terms.
func jobCountsLabel(j jobCounts) string {
	parts := []string{}
	if j.Running > 0 {
		parts = append(parts, runningStyle.Render(fmt.Sprintf("%dr", j.Running)))
	}
	if j.Queued > 0 {
		parts = append(parts, queuedStyle.Render(fmt.Sprintf("%dq", j.Queued)))
	}
	if j.Unplaced > 0 {
		parts = append(parts, failedStyle.Render(fmt.Sprintf("%du", j.Unplaced)))
	}
	if len(parts) == 0 {
		return dimStyle.Render("—")
	}
	return strings.Join(parts, dimStyle.Render(" + "))
}

type providerInfo struct {
	count int
	rate  float64 // $/hr across live instances on this provider
}

// jobCounts is the per-destination breakdown shown in the Flow view.
type jobCounts struct {
	Running  int
	Queued   int
	Unplaced int // sub-category of queued; jobs not yet assigned to anything
}

type flowData struct {
	totalJobs int
	byDest    map[string]jobCounts
	providers map[string]providerInfo
}

func buildFlow(snap Snapshot) flowData {
	out := flowData{
		byDest:    map[string]jobCounts{},
		providers: map[string]providerInfo{},
	}
	for _, j := range snap.Jobs {
		if j == nil {
			continue
		}
		dest := destLabelForJob(j)
		jc := out.byDest[dest]
		switch j.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			jc.Running++
		case db.StatusQueued, db.StatusPendingPlacement:
			if j.TargetKind() == db.JobTargetUnplaced {
				jc.Unplaced++
			} else {
				jc.Queued++
			}
		default:
			continue
		}
		out.byDest[dest] = jc
		out.totalJobs++
	}
	for _, l := range snap.LiveInstances {
		if l == nil {
			continue
		}
		info := out.providers[l.Provider]
		info.count++
		info.rate += float64(l.CostPerHourCents) / 100.0
		out.providers[l.Provider] = info
	}
	return out
}

func destLabelForJob(j *db.Job) string {
	switch j.TargetKind() {
	case db.JobTargetInventoryHost:
		return "on-prem " + j.Host
	case db.JobTargetRentalInstance:
		if j.LaunchID != nil {
			return fmt.Sprintf("rental wi%d", *j.LaunchID)
		}
		return "rental ?"
	default:
		return "(unplaced)"
	}
}

// providerLabelForDest returns the right-hand provider+cost tag for a
// destination label produced by destLabelForJob. Returns "" when the
// destination is on-prem or unplaced (no provider chain).
func providerLabelForDest(dest string, snap Snapshot) string {
	if !strings.HasPrefix(dest, "rental wi") {
		return ""
	}
	idStr := strings.TrimPrefix(dest, "rental wi")
	var launchID int64
	if _, err := fmt.Sscanf(idStr, "%d", &launchID); err != nil {
		return ""
	}
	for _, l := range append(snap.LiveInstances, snap.StaleInstances...) {
		if l == nil || l.ID != launchID {
			continue
		}
		rate := float64(l.CostPerHourCents) / 100.0
		status := instanceDisplayStatus(l)
		var statusTag string
		switch status {
		case "running":
			statusTag = ""
		case "launching":
			statusTag = dimStyle.Render(" (launching)")
		default:
			statusTag = dimStyle.Render(" (" + status + ")")
		}
		return fmt.Sprintf("%s · %s%s",
			accentStyle.Render(l.Provider),
			runningStyle.Render(fmt.Sprintf("$%.2f/hr", rate)),
			statusTag)
	}
	return dimStyle.Render("(launch not found)")
}
