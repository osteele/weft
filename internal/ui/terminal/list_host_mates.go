package terminal

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/osteele/weft/internal/db"
)

// hostMateMarker is the glyph painted at the left edge of rows that share
// a host with the selected row. It is a half-cell vertical bar (U+258E) that
// is one terminal cell wide. The marker overlays the first cell of the row
// rather than reserving its own gutter, so column widths are unchanged
// regardless of whether highlighting is active.
const hostMateMarker = "▎"

var hostMateMarkerStyle = lipgloss.NewStyle().Foreground(tuiOnPremColor)
var hostMateRentalMarkerStyle = lipgloss.NewStyle()

// hostMateKey returns a stable identifier for the host a job is placed on,
// or "" if the job is unplaced or has no resolvable host. Unplaced jobs do
// not match each other — being collectively unplaced is not a meaningful
// "same host" relationship.
func hostMateKey(job *db.Job) string {
	if job == nil {
		return ""
	}
	switch job.TargetKind() {
	case db.JobTargetRentalInstance:
		if job.LaunchID != nil && *job.LaunchID > 0 {
			return "rental:" + formatRentalInstanceLabel(job)
		}
		return ""
	case db.JobTargetInventoryHost:
		host := strings.TrimSpace(job.Host)
		if host == "" {
			return ""
		}
		return "host:" + host
	}
	return ""
}

// hostMatesForFlatView returns the set of row indices whose job shares a
// host with the cursor's job, excluding the cursor row itself. ok is true
// only when at least one mate exists.
func hostMatesForFlatView(jobs []*db.Job, cursor int) (mates map[int]bool, ok bool) {
	if cursor < 0 || cursor >= len(jobs) {
		return nil, false
	}
	key := hostMateKey(jobs[cursor])
	if key == "" {
		return nil, false
	}
	mates = make(map[int]bool, 4)
	for i, j := range jobs {
		if i == cursor {
			continue
		}
		if hostMateKey(j) == key {
			mates[i] = true
		}
	}
	if len(mates) == 0 {
		return nil, false
	}
	return mates, true
}

// hostMatesForGroupedView returns the set of job IDs that share a host with
// the selected job, excluding the selected job itself.
func hostMatesForGroupedView(selected *db.Job, jobs []*db.Job) (mates map[int64]bool, ok bool) {
	if selected == nil {
		return nil, false
	}
	key := hostMateKey(selected)
	if key == "" {
		return nil, false
	}
	mates = make(map[int64]bool, 4)
	for _, j := range jobs {
		if j == nil || j.ID == selected.ID {
			continue
		}
		if hostMateKey(j) == key {
			mates[j.ID] = true
		}
	}
	if len(mates) == 0 {
		return nil, false
	}
	return mates, true
}

// hostMatesForGroupedRows returns grouped row indices that share a host with
// the selected row. Grouped status views can render multiple rows for one
// logical job during a pending move, so row identity must include the display
// attempt and dim state rather than only the job ID.
func hostMatesForGroupedRows(rows []groupedStatusRow, selectedRow int) (mates map[int]bool, ok bool) {
	if selectedRow < 0 || selectedRow >= len(rows) {
		return nil, false
	}
	selected := rows[selectedRow].job
	key := hostMateKey(selected)
	if key == "" {
		return nil, false
	}
	mates = make(map[int]bool, 4)
	for i, row := range rows {
		if i == selectedRow || row.job == nil || row.isHeader || row.isBlocked {
			continue
		}
		if sameGroupedDisplayRow(row.job, selected) {
			continue
		}
		if hostMateKey(row.job) == key {
			mates[i] = true
		}
	}
	if len(mates) == 0 {
		return nil, false
	}
	return mates, true
}

func sameGroupedDisplayRow(a, b *db.Job) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID &&
		a.DisplayAttemptID == b.DisplayAttemptID &&
		a.DisplayMoveDim == b.DisplayMoveDim &&
		hostMateKey(a) == hostMateKey(b)
}

// applyHostMateMarker overlays the marker on the first display cell of line,
// preserving the line's overall visual width so columns stay aligned with
// rows that aren't host-mates. If line is empty, just the styled marker is
// returned.
func applyHostMateMarker(line string) string {
	return applyStyledHostMateMarker(line, hostMateMarkerStyle.Render(hostMateMarker))
}

func applyHostMateMarkerForJob(line string, job *db.Job) string {
	styled := hostMateRentalMarkerStyle.Render(hostMateMarker)
	if job != nil && job.UsesInventoryPlacement() {
		styled = hostMateMarkerStyle.Render(hostMateMarker)
	}
	return applyStyledHostMateMarker(line, styled)
}

func applyStyledHostMateMarker(line, styled string) string {
	if ansi.StringWidth(line) == 0 {
		return styled
	}
	return styled + ansi.TruncateLeft(line, 1, "")
}
