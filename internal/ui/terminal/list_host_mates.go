package terminal

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

// hostMateMarker is the glyph painted at the left edge of rows that share
// a host with the selected row. It is a half-cell vertical bar (U+258E) that
// is one terminal cell wide. The marker overlays the first cell of the row
// rather than reserving its own gutter, so column widths are unchanged
// regardless of whether highlighting is active.
const hostMateMarker = "▎"

var hostMateMarkerStyle = lipgloss.NewStyle().Foreground(tuiAccentColor)

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

// applyHostMateMarker overlays the marker on the first display cell of line,
// preserving the line's overall visual width so columns stay aligned with
// rows that aren't host-mates. If line is empty, just the styled marker is
// returned.
func applyHostMateMarker(line string) string {
	styled := hostMateMarkerStyle.Render(hostMateMarker)
	runes := []rune(line)
	if len(runes) == 0 {
		return styled
	}
	return styled + string(runes[1:])
}
