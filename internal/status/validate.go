package status

import "log/slog"

// WarnOnInvalid validates a status transition and logs a warning if invalid.
// Returns true if the transition is valid (or a no-op), false if invalid.
// This is the warn-mode entry point: callers should proceed even on false
// to avoid breaking production during the burn-in period.
func WarnOnInvalid(from, to string, authoritative bool, source Source) bool {
	if from == to {
		return true
	}
	_, err := ValidateTransition(from, to, authoritative)
	if err != nil {
		slog.Warn("invalid status transition",
			"from", from,
			"to", to,
			"authoritative", authoritative,
			"source", source,
			"error", err.Error(),
		)
		return false
	}
	return true
}
