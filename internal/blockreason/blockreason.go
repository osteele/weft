package blockreason

import (
	"regexp"
	"strings"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

// Source identifies where a displayed blocker came from.
type Source string

const (
	SourceNone      Source = ""
	SourceLive      Source = "live"
	SourceAutoPilot Source = "autopilot"
	SourcePlacement Source = "placement"
)

// Kind identifies how a visible reason should be labeled in compact UI.
type Kind string

const (
	KindNone    Kind = ""
	KindBlocked Kind = "blocked"
	KindWaiting Kind = "waiting"
)

// Options controls blocker resolution for a UI surface.
type Options struct {
	// AutoPilotReason is the latest in-memory blocker returned by an
	// auto-pilot pass. It is current state, unlike PlacementReasons.
	AutoPilotReason string

	// CloudConfigured hides routine on-prem rejection handoff reasons from
	// detail surfaces when rentals are available.
	CloudConfigured bool

	// Compact hides non-actionable history from at-a-glance labels.
	Compact bool
}

// Result is the resolved current blocker for a compact UI label.
type Result struct {
	Blocked bool
	Kind    Kind
	Reason  string
	Source  Source
}

// Resolve returns the current blocker that should appear in a compact label.
// Persisted placement history is considered only while the job is unplaced;
// a placed queued job can still be waiting, but old placement failures are no
// longer the reason it is blocked.
func Resolve(job *db.Job, opts Options) Result {
	if job == nil {
		return Result{}
	}
	if reason := visibleReason(job, job.QueueBlockedReason, opts); reason != "" {
		return Result{Blocked: true, Kind: ReasonKind(reason), Reason: reason, Source: SourceLive}
	}
	if reason := visibleReason(job, opts.AutoPilotReason, opts); reason != "" {
		return Result{Blocked: true, Kind: ReasonKind(reason), Reason: reason, Source: SourceAutoPilot}
	}
	if !job.IsUnplacedAwaitingPlacement() {
		return Result{}
	}
	if reason := LatestPlacementReason(job, opts); reason != "" {
		return Result{Blocked: true, Kind: ReasonKind(reason), Reason: reason, Source: SourcePlacement}
	}
	return Result{}
}

// ReasonKind classifies a visible reason for compact UI labels.
func ReasonKind(reason string) Kind {
	switch strings.TrimSpace(campaign.SanitizeBlockedReason(reason)) {
	case "daemon placement pending",
		"inventory-tagged: waiting for on-prem host":
		return KindWaiting
	case "":
		return KindNone
	default:
		return KindBlocked
	}
}

// Reasons returns all visible blocker reasons for a detail view.
func Reasons(job *db.Job, opts Options) []string {
	if job == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(reason string) {
		reason = visibleReason(job, reason, opts)
		if reason == "" {
			return
		}
		if _, ok := seen[reason]; ok {
			return
		}
		seen[reason] = struct{}{}
		out = append(out, reason)
	}
	add(job.QueueBlockedReason)
	add(opts.AutoPilotReason)
	for _, reason := range job.PlacementReasons {
		add(reason)
	}
	return out
}

// LatestPlacementReason returns the most recent visible persisted placement
// reason. If the newest non-empty reason is hidden as routine history, older
// reasons are intentionally not resurrected.
func LatestPlacementReason(job *db.Job, opts Options) string {
	if job == nil {
		return ""
	}
	for i := len(job.PlacementReasons) - 1; i >= 0; i-- {
		reason := campaign.SanitizeBlockedReason(job.PlacementReasons[i])
		if reason == "" {
			continue
		}
		return visibleReason(job, reason, opts)
	}
	return ""
}

func visibleReason(job *db.Job, reason string, opts Options) string {
	reason = campaign.SanitizeBlockedReason(reason)
	if opts.Compact {
		reason = StripContractRef(reason)
	}
	if reason == "" {
		return ""
	}
	if isUnplacedResetReason(reason) {
		return ""
	}
	if shouldHideOnPremRejection(job, reason, opts) {
		return ""
	}
	return reason
}

func shouldHideOnPremRejection(job *db.Job, reason string, opts Options) bool {
	if !isOnPremRejectionReason(reason) {
		return false
	}
	if opts.CloudConfigured {
		return true
	}
	return job != nil && !job.HasTag(db.TagInventory)
}

var contractRefPattern = regexp.MustCompile(`\s*\(contract \d+\)`)

// StripContractRef removes provider contract ids from compact display text.
func StripContractRef(reason string) string {
	return strings.TrimSpace(contractRefPattern.ReplaceAllString(reason, ""))
}

var onPremHostsRejectionPattern = regexp.MustCompile(`^\d+ hosts?: `)

func isOnPremRejectionReason(reason string) bool {
	r := strings.TrimSpace(reason)
	if strings.HasPrefix(r, "no local host matched ") {
		return true
	}
	return onPremHostsRejectionPattern.MatchString(r)
}

var cloudInstanceReturnedToQueuePattern = regexp.MustCompile(`^cloud instance \d+ (failed(?: \([^)]+\))?|was canceled|returned job to queue(?: \([^)]+\))?)$`)

func isUnplacedResetReason(reason string) bool {
	r := strings.TrimSpace(reason)
	if r == "replan requested; previous rental placement canceled" {
		return true
	}
	if cloudInstanceReturnedToQueuePattern.MatchString(r) {
		return true
	}
	return strings.Contains(r, "unplaced queue") &&
		(strings.Contains(r, "job reset to unplaced queue") ||
			strings.Contains(r, "job returned to unplaced queue") ||
			strings.Contains(r, "returned from cloud instance to unplaced queue"))
}
