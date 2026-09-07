package opsqueue

import "strings"

const (
	missingRunnerCapabilityPrefix    = "queue runner lacks "
	missingRunnerCapabilitySeparator = " capability;"
)

// MissingRunnerCapabilityBlockDetail formats a dispatch detail recognized by
// IsMissingRunnerCapabilityBlock.
func MissingRunnerCapabilityBlockDetail(capability, remedy string) string {
	return missingRunnerCapabilityPrefix + strings.TrimSpace(capability) +
		missingRunnerCapabilitySeparator + " " + strings.TrimSpace(remedy)
}

// IsMissingRunnerCapabilityBlock reports whether a dispatch detail identifies
// a queue runner that cannot accept the job because it lacks a capability.
func IsMissingRunnerCapabilityBlock(detail string) bool {
	detail = strings.TrimSpace(detail)
	start := strings.Index(detail, missingRunnerCapabilityPrefix)
	if start < 0 {
		return false
	}
	if start > 0 && !strings.HasSuffix(detail[:start], ": ") {
		return false
	}
	remainder := detail[start+len(missingRunnerCapabilityPrefix):]
	capability, remedy, ok := strings.Cut(remainder, missingRunnerCapabilitySeparator)
	return ok && strings.TrimSpace(capability) != "" && strings.TrimSpace(remedy) != ""
}
