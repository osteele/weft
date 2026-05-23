package dashboard

// This file exposes the dashboard's host-row rendering helpers for reuse by
// the `weft host list --tui` panel under internal/ui/terminal/. They are
// otherwise identical to the unexported helpers in render_hosts.go.

// HostSummaryFormat selects the label style for a one-line host summary.
type HostSummaryFormat = hostSummaryFormat

const (
	// HostSummaryFull renders full-word labels (e.g. "CPU 45%").
	HostSummaryFull HostSummaryFormat = hostSummaryFull
	// HostSummaryAbbrev renders single-letter labels (e.g. "C45%").
	HostSummaryAbbrev HostSummaryFormat = hostSummaryAbbrev
)

// RenderHostSummarySegment returns a styled one-line summary for a host:
// status glyph, name, and CPU/MEM/GPU/disk percentages. Inputs and styling
// are identical to the dashboard's internal renderer.
func RenderHostSummarySegment(host *Host, format HostSummaryFormat) string {
	return Model{}.renderHostSummarySegment(host, format)
}
