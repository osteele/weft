package blockkind

import "strings"

type Kind string

const (
	KindNone    Kind = ""
	KindBlocked Kind = "blocked"
	KindWaiting Kind = "waiting"
	KindPaused  Kind = "paused"
)

// ReasonKind classifies a visible blocker reason for compact UI labels.
func ReasonKind(reason string) Kind {
	cleaned := strings.TrimSpace(NormalizeOfferFetchUnavailable(reason))
	switch {
	case isPausedReason(cleaned):
		return KindPaused
	case cleaned == "placement pending",
		cleaned == "autopilot",
		cleaned == "autopilot placing jobs",
		cleaned == "autopilot paused",
		cleaned == "autopilot not running",
		strings.HasPrefix(cleaned, "autopilot delayed "),
		isOfferFetchUnavailable(cleaned),
		isRetryableOfferUnavailable(cleaned),
		isRetryableCreateProviderRejection(cleaned),
		isRetryableProviderReachabilityFailure(cleaned),
		cleaned == "inventory-tagged: waiting for on-prem host",
		strings.Contains(cleaned, "source sync already in flight"),
		strings.Contains(cleaned, "source sync backing off"),
		strings.Contains(cleaned, "source sync deferred"),
		strings.Contains(cleaned, "source sync failed"),
		isIncompleteProducerWait(cleaned):
		return KindWaiting
	case cleaned == "":
		return KindNone
	default:
		return KindBlocked
	}
}

func DisplayReasonForKind(kind Kind, reason string) string {
	reason = strings.TrimSpace(NormalizeOfferFetchUnavailable(reason))
	if kind != KindPaused {
		return reason
	}
	reason = strings.TrimSpace(strings.TrimPrefix(reason, "new-instance retry blocked:"))
	reason = strings.TrimSpace(strings.TrimPrefix(reason, "new-instance retry paused:"))
	reason = strings.TrimSpace(strings.TrimPrefix(reason, "paused:"))
	return reason
}

func NormalizeOfferFetchUnavailable(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" || strings.Contains(reason, "market unknown") {
		return reason
	}
	const prefix = "offer fetch unavailable"
	const plannerPrefix = "planner: " + prefix
	if reason == prefix {
		return "provider offer fetch unavailable (market unknown; Weft will retry)"
	}
	if reason == plannerPrefix {
		return "planner: provider offer fetch unavailable (market unknown; Weft will retry)"
	}
	if detail, ok := strings.CutPrefix(reason, prefix+":"); ok {
		detail = strings.TrimSpace(detail)
		if detail == "" {
			return "provider offer fetch unavailable (market unknown; Weft will retry)"
		}
		return "provider offer fetch unavailable: " + detail + "; market unknown; Weft will retry"
	}
	if detail, ok := strings.CutPrefix(reason, plannerPrefix+":"); ok {
		detail = strings.TrimSpace(detail)
		if detail == "" {
			return "planner: provider offer fetch unavailable (market unknown; Weft will retry)"
		}
		return "planner: provider offer fetch unavailable: " + detail + "; market unknown; Weft will retry"
	}
	return reason
}

func isPausedReason(reason string) bool {
	reason = strings.TrimSpace(reason)
	return strings.HasPrefix(reason, "paused:") ||
		strings.HasPrefix(reason, "new-instance retry paused:") ||
		strings.HasPrefix(reason, "new-instance retry blocked: paused:")
}

func isOfferFetchUnavailable(reason string) bool {
	return strings.Contains(reason, "offer fetch unavailable") &&
		strings.Contains(reason, "market unknown") &&
		strings.Contains(reason, "Weft will retry")
}

func isRetryableOfferUnavailable(reason string) bool {
	return strings.Contains(reason, "offer unavailable:") &&
		strings.Contains(reason, "Weft will retry with fresh offers")
}

func isRetryableCreateProviderRejection(reason string) bool {
	lower := strings.ToLower(reason)
	return strings.Contains(lower, "provider rejected request") &&
		strings.Contains(lower, "create-instance") &&
		strings.Contains(reason, "Weft will retry with fresh offers")
}

func isRetryableProviderReachabilityFailure(reason string) bool {
	return strings.Contains(reason, "unreachable") &&
		strings.Contains(reason, "network or provider outage") &&
		strings.Contains(reason, "Weft will retry")
}

func isIncompleteProducerWait(reason string) bool {
	reason = strings.TrimSpace(reason)
	if !strings.HasPrefix(reason, "waiting for ") || !strings.Contains(reason, " from wj") {
		return false
	}
	if beforeDetail, _, ok := strings.Cut(reason, ";"); ok {
		reason = strings.TrimSpace(beforeDetail)
	}
	return strings.HasSuffix(reason, "(queued)") ||
		strings.HasSuffix(reason, "(running)") ||
		strings.HasSuffix(reason, "(starting)")
}
