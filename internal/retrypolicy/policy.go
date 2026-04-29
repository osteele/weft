// Package retrypolicy defines the per-job relaunch backoff schedule.
package retrypolicy

import "time"

var backoffDelays = []time.Duration{
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
	2 * time.Minute,
}

func BackoffDelay(attempt int) (time.Duration, bool) {
	if attempt < 0 || attempt >= len(backoffDelays) {
		return 0, false
	}
	return backoffDelays[attempt], true
}

func BackoffDelayClamped(attempt int) time.Duration {
	switch {
	case len(backoffDelays) == 0:
		return 0
	case attempt < 0:
		return backoffDelays[0]
	case attempt >= len(backoffDelays):
		return backoffDelays[len(backoffDelays)-1]
	default:
		return backoffDelays[attempt]
	}
}

func MaxAttempts() int {
	return len(backoffDelays) + 1
}
