package terminal

import "time"

const tuiUnfocusedThrottleMultiplier = 6

func throttledInterval(base time.Duration, focused bool) time.Duration {
	if focused || base <= 0 {
		return base
	}
	return base * tuiUnfocusedThrottleMultiplier
}
