package terminal

import "github.com/osteele/weft/internal/ui/hit"

type targetKind uint8

const (
	targetNone targetKind = iota
	targetSelectRow
	targetToggleSection
	targetOpenURL
	targetRestartDaemon
	targetCopy
	targetCollapseBlocked
	targetIncidentJump
	targetAutoErrorToggle
	targetExpandAllBlockers
	targetDiagnose
	targetInstanceFailures
	targetInstances
	targetHosts
	targetDismissStatus
	targetControlsKey
)

type clickTarget struct {
	kind    targetKind
	rowIdx  int // index into m.groupedRows (grouped view) or m.jobs (flat view); -1 otherwise
	toggle  string
	url     string
	label   string
	payload string // copy payload when it is not derived from a grouped row
	jobID   int64  // job a disclosure-collapse target belongs to
	key     string // controls-line key token to invoke
}

func (t clickTarget) actionable() bool { return t.kind != targetNone }

type hitSpan = hit.Span[clickTarget]
type screenLine = hit.Line[clickTarget]
type screenPlan = hit.Plan[clickTarget]

// newScreenPlan returns an empty plan sized for a width x height terminal.
func newScreenPlan(width, height int) screenPlan {
	return hit.New[clickTarget](width, height)
}
