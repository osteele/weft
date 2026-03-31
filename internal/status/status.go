// Package status defines job attempt status values, allowed transitions between
// them, and validation logic. It is pure domain logic with no database dependency.
//
// The transition table is the single source of truth for which status changes are
// legal. DB mutation functions call ValidateTransition before executing SQL.
package status

import "fmt"

// Status constants for job attempts.
const (
	Starting         = "starting"
	Running          = "running"
	Completed        = "completed"
	Dead             = "dead"
	Queued           = "queued"
	Failed           = "failed"
	Killed           = "killed"
	Canceled         = "canceled"
	Paused           = "paused"
	Draft            = "draft"
	PendingPlacement = "pending_placement"
)

// AllStatuses returns every valid status value.
func AllStatuses() []string {
	return []string{
		Starting, Running, Completed, Dead, Queued,
		Failed, Killed, Canceled, Paused, Draft, PendingPlacement,
	}
}

// terminalStatuses is the set of statuses from which no normal (non-authoritative)
// forward progress is expected.
var terminalStatuses = map[string]bool{
	Completed: true,
	Dead:      true,
	Failed:    true,
	Killed:    true,
	Canceled:  true,
	Draft:     true,
}

// IsTerminal reports whether s is a terminal status.
func IsTerminal(s string) bool {
	return terminalStatuses[s]
}

// Source identifies what signal triggered a status transition.
type Source string

const (
	SourceSSHSync      Source = "ssh_sync"
	SourceSlurmSync    Source = "slurm_sync"
	SourceR2Completion Source = "r2_completion"
	SourceR2Phase      Source = "r2_phase"
	SourceReconcile    Source = "reconcile"
	SourceUserAction   Source = "user_action"
	SourceCleanup      Source = "cleanup"
	SourceQueueRunner  Source = "queue_runner"
	SourcePlacement    Source = "placement"
)

// TransitionRule describes one allowed status transition.
type TransitionRule struct {
	From          string
	To            string
	UpdatesSynced bool // whether last_synced_status should also be updated
	Authoritative bool // whether this can override terminal states
}

// transitions is the master table of allowed status changes, derived from
// auditing the WHERE clauses and caller behavior of every DB mutation function.
var transitions = []TransitionRule{
	// --- From queued ---
	{From: Queued, To: Starting, UpdatesSynced: false},
	{From: Queued, To: Running, UpdatesSynced: true},
	{From: Queued, To: Completed, UpdatesSynced: true, Authoritative: true},
	{From: Queued, To: Canceled, UpdatesSynced: true},
	{From: Queued, To: Killed, UpdatesSynced: true},
	{From: Queued, To: Dead, UpdatesSynced: true},
	{From: Queued, To: Failed, UpdatesSynced: true},
	{From: Queued, To: Paused, UpdatesSynced: false},
	{From: Queued, To: Draft, UpdatesSynced: false},

	// --- From pending_placement ---
	{From: PendingPlacement, To: Queued, UpdatesSynced: false},
	{From: PendingPlacement, To: Canceled, UpdatesSynced: false},

	// --- From starting ---
	{From: Starting, To: Running, UpdatesSynced: false},
	{From: Starting, To: Completed, UpdatesSynced: true},
	{From: Starting, To: Dead, UpdatesSynced: true},
	{From: Starting, To: Failed, UpdatesSynced: true},
	{From: Starting, To: Queued, UpdatesSynced: false},

	// --- From running ---
	{From: Running, To: Completed, UpdatesSynced: true},
	{From: Running, To: Failed, UpdatesSynced: true},
	{From: Running, To: Dead, UpdatesSynced: true},
	{From: Running, To: Killed, UpdatesSynced: true},
	{From: Running, To: Canceled, UpdatesSynced: true},
	{From: Running, To: Paused, UpdatesSynced: false},
	{From: Running, To: Queued, UpdatesSynced: false},

	// --- From paused ---
	{From: Paused, To: Running, UpdatesSynced: false},
	{From: Paused, To: Killed, UpdatesSynced: true},
	{From: Paused, To: Failed, UpdatesSynced: true},
	{From: Paused, To: Queued, UpdatesSynced: false},

	// --- From terminal states (restart/requeue paths) ---
	{From: Failed, To: Running, UpdatesSynced: false},
	{From: Failed, To: Paused, UpdatesSynced: false},
	{From: Failed, To: Queued, UpdatesSynced: false},
	{From: Dead, To: Running, UpdatesSynced: false},
	{From: Dead, To: Paused, UpdatesSynced: false},
	{From: Dead, To: Queued, UpdatesSynced: false},
	{From: Killed, To: Paused, UpdatesSynced: false},
	{From: Killed, To: Queued, UpdatesSynced: false},
	{From: Canceled, To: Paused, UpdatesSynced: false},
	{From: Canceled, To: Queued, UpdatesSynced: false},

	// --- Authoritative overrides (R2 completion can fix race conditions) ---
	{From: Failed, To: Completed, UpdatesSynced: true, Authoritative: true},
	{From: Dead, To: Completed, UpdatesSynced: true, Authoritative: true},
	{From: Killed, To: Completed, UpdatesSynced: true, Authoritative: true},
	{From: Canceled, To: Completed, UpdatesSynced: true, Authoritative: true},

	// --- To draft (user pulls job back to local-only) ---
	{From: Starting, To: Draft, UpdatesSynced: false},
	{From: Running, To: Draft, UpdatesSynced: false},

	// --- From draft ---
	{From: Draft, To: Queued, UpdatesSynced: false},
}

// transitionIndex is built at init time for O(1) lookup.
// Key: "from\x00to", value: pointer into transitions slice.
var transitionIndex map[string]*TransitionRule

func init() {
	transitionIndex = make(map[string]*TransitionRule, len(transitions))
	for i := range transitions {
		key := transitions[i].From + "\x00" + transitions[i].To
		transitionIndex[key] = &transitions[i]
	}
}

// ValidateTransition checks whether transitioning from→to is allowed.
// If authoritative is true, authoritative rules (which can override terminal
// states) are also considered. Returns the matching rule on success.
func ValidateTransition(from, to string, authoritative bool) (*TransitionRule, error) {
	if from == to {
		return nil, nil // no-op, always allowed
	}
	key := from + "\x00" + to
	rule, ok := transitionIndex[key]
	if !ok {
		return nil, &InvalidTransitionError{From: from, To: to, Reason: "no rule exists for this transition"}
	}
	if rule.Authoritative && !authoritative {
		return nil, &InvalidTransitionError{From: from, To: to, Reason: "transition requires authoritative source"}
	}
	return rule, nil
}

// AllowedFrom returns all statuses reachable from the given status.
// If includeAuthoritative is true, authoritative transitions are included.
func AllowedFrom(from string, includeAuthoritative bool) []string {
	var result []string
	for i := range transitions {
		if transitions[i].From == from && (includeAuthoritative || !transitions[i].Authoritative) {
			result = append(result, transitions[i].To)
		}
	}
	return result
}

// InvalidTransitionError is returned when a status transition is not allowed.
type InvalidTransitionError struct {
	From   string
	To     string
	Reason string
}

func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("invalid status transition %s -> %s: %s", e.From, e.To, e.Reason)
}
