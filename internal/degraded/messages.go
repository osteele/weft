package degraded

import "fmt"

const (
	HostStatusStale = "stale"
)

func CloudLastKnownRentalsReason() string {
	return "using last-known rental instances (degraded data)"
}

func CloudSyncTimedOutShowingLastKnownState(timeout string) string {
	return fmt.Sprintf("cloud sync timed out after %s; showing last known state", timeout)
}

func CloudSyncTimedOutWaitingForDB(timeout string) string {
	return fmt.Sprintf("Cloud sync timed out after %s; waiting for DB updates.", timeout)
}

// CloudStateStalePendingRefresh is shown after rendering when a background
// cloud sync is still in flight past the soft deadline. The caller blocks on
// the sync before returning, so the shell will appear to pause briefly.
func CloudStateStalePendingRefresh() string {
	return "Cloud state may be stale; refreshing in background..."
}

func TerminalJobAttachmentDegradedNote() string {
	return "Note: some terminal instance rows are using last-known job attachment (degraded data)."
}

func TerminalJobAttachmentDegradedFooter() string {
	return "using last-known terminal job attachment"
}
