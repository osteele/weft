package terminal

import (
	"fmt"
	"time"
)

const tuiUnfocusedThrottleMultiplier = 6

func throttledInterval(base time.Duration, focused bool) time.Duration {
	if focused || base <= 0 {
		return base
	}
	return base * tuiUnfocusedThrottleMultiplier
}

func waitUntil(t time.Time) time.Duration {
	wait := time.Until(t).Round(time.Second)
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

func formatAutoPilotNextPass(until time.Time, unplaced int) string {
	return fmt.Sprintf("Auto-pilot: next pass in %s (%d unplaced)", waitUntil(until), unplaced)
}
