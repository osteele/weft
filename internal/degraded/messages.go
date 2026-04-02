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

func TerminalJobAttachmentDegradedNote() string {
	return "Note: some terminal instance rows are using last-known job attachment (degraded data)."
}

func TerminalJobAttachmentDegradedFooter() string {
	return "using last-known terminal job attachment"
}
