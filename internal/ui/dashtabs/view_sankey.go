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

// Render shows job flow as a left-to-right tree:
//
//	running (2)
//	  ├─→ rental wi3289 (1) ──→ vastai · $1.94/hr
//	  └─→ rental wi3294 (1) ──→ vastai · $0.33/hr
//	queued (9)
//	  ├─→ rental wi3297 (3) ──→ vastai (launching)
//	  ├─→ rental wi3294 (2)
//	  └─→ (unplaced)   (1)
//
// Source on the left branches into destinations, which chain to provider +
// $/hr on the right. Each source row keeps its sub-tree of destinations
// indented under it; tree connectors are real Unicode box-drawing chars so
// it reads as a single graph rather than three independent columns.
func (v *sankeyView) Render(width, height int, snap Snapshot, _ bool) string {
	flow := buildFlow(snap)
	if flow.totalJobs == 0 && len(flow.providers) == 0 {
		return dimStyle.Render("Flow — no jobs to route.")
	}

	var b strings.Builder
	b.WriteString(titleStyle.Render("Flow — status → destination → provider"))
	b.WriteString("\n\n")

	type srcRow struct {
		name  string
		count int
		style func(...string) string
	}
	sources := []srcRow{
		{"running", flow.running, runningStyle.Render},
		{"queued", flow.queued, queuedStyle.Render},
	}
	if flow.unplaced > 0 {
		// "unplaced" is a subset of queued, but worth surfacing as its own
		// source row because it implies "no destination at all yet".
		sources = append(sources, srcRow{"unplaced", flow.unplaced, failedStyle.Render})
	}

	// Auto-size the destination column to the longest label across all
	// sources, plus a small fixed buffer. Avoids the "valley" that comes
	// from always reserving a worst-case width.
	destColW := 0
	for _, dests := range flow.bySource {
		for dest, count := range dests {
			w := len(fmt.Sprintf("%s (%d)", dest, count))
			if w > destColW {
				destColW = w
			}
		}
	}
	destColW += 2 // breathing room before the connector

	for _, src := range sources {
		if src.count == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("%s\n", src.style(fmt.Sprintf("%s (%d)", src.name, src.count))))

		dests := flow.bySource[src.name]
		if len(dests) == 0 {
			b.WriteString(dimStyle.Render("    └─→ (no routing info)"))
			b.WriteString("\n\n")
			continue
		}
		destKeys := make([]string, 0, len(dests))
		for k := range dests {
			destKeys = append(destKeys, k)
		}
		// Sort: rentals before on-prem before unplaced, then by count desc.
		sort.Slice(destKeys, func(i, j int) bool {
			a, c := destKeys[i], destKeys[j]
			if dests[a] != dests[c] {
				return dests[a] > dests[c]
			}
			return a < c
		})

		// Per the design mockup, every leaf uses the corner glyph (└─→)
		// rather than mixing ├─→ / └─→ — visually it reads cleaner and
		// makes each destination feel like an independent branch.
		for _, dest := range destKeys {
			destLabel := fmt.Sprintf("%s (%d)", dest, dests[dest])
			// `─` (U+2500) and `→` (U+2192) come from different Unicode
			// blocks and rarely share a vertical centerline in monospace
			// fonts. The black right-pointing triangle (U+25B6) is in the
			// geometric-shapes block, which most fonts position to match
			// box-drawing characters — so `──▶` reads as a continuous
			// connector rather than two staggered glyphs.
			line := fmt.Sprintf("    %s %s",
				dimStyle.Render("└─▶"),
				accentStyle.Render(padRight(destLabel, destColW)))
			if provLabel := providerLabelForDest(dest, snap); provLabel != "" {
				line += "  " + dimStyle.Render("──▶") + "  " + provLabel
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	_ = width
	_ = height
	return b.String()
}

type providerInfo struct {
	count int
	rate  float64 // $/hr across live instances on this provider
}

type flowData struct {
	totalJobs       int
	running, queued int
	unplaced        int
	// bySource[srcName][destLabel] = job count
	bySource  map[string]map[string]int
	providers map[string]providerInfo
}

func buildFlow(snap Snapshot) flowData {
	out := flowData{
		bySource:  map[string]map[string]int{},
		providers: map[string]providerInfo{},
	}
	addEdge := func(src, dest string) {
		if _, ok := out.bySource[src]; !ok {
			out.bySource[src] = map[string]int{}
		}
		out.bySource[src][dest]++
	}
	for _, j := range snap.Jobs {
		if j == nil {
			continue
		}
		es := j.EffectiveStatus()
		var srcName string
		switch es {
		case db.StatusRunning, db.StatusStarting:
			out.running++
			srcName = "running"
		case db.StatusQueued, db.StatusPendingPlacement:
			out.queued++
			if j.TargetKind() == db.JobTargetUnplaced {
				out.unplaced++
				srcName = "unplaced"
			} else {
				srcName = "queued"
			}
		default:
			continue
		}
		out.totalJobs++

		dest := destLabelForJob(j)
		addEdge(srcName, dest)
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
	// Parse the launch ID out of "rental wiNNN".
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

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
