package campaign

import "github.com/osteele/weft/internal/db"

// DisplayInstanceStatus returns the canonical status label for an instance.
// It maps terminal failure/cancel statuses to "terminated" and preserves
// grace labels.
func DisplayInstanceStatus(ci *db.Launch) string {
	if ci == nil {
		return ""
	}
	if label := ci.GraceStatusLabel(); label != "" {
		return label
	}
	switch ci.Status {
	case db.LaunchStatusFailed, db.LaunchStatusCancelled:
		return "terminated"
	default:
		return ci.Status
	}
}

// DisplayTerminationReason returns a canonical human-readable termination
// reason label. It hides completed reasons and normalizes job failure wording.
func DisplayTerminationReason(ci *db.Launch) string {
	if ci == nil || ci.TerminationReason == "" || ci.TerminationReason == db.TerminationReasonCompleted {
		return ""
	}
	if ci.TerminationReason == db.TerminationReasonJobFailure {
		return "job failed"
	}
	return db.HumanizeTerminationReason(ci.TerminationReason)
}

// DisplayInstanceStatusWithReason returns the canonical status label with
// an appended reason when available.
func DisplayInstanceStatusWithReason(ci *db.Launch) string {
	status := DisplayInstanceStatus(ci)
	if reason := DisplayTerminationReason(ci); reason != "" {
		return status + " (" + reason + ")"
	}
	return status
}
