package orchestration

import "time"

var retryBackoffDelays = []time.Duration{
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
	2 * time.Minute,
}

func RetryBackoffDelay(attempt int) (time.Duration, bool) {
	if attempt < 0 || attempt >= len(retryBackoffDelays) {
		return 0, false
	}
	return retryBackoffDelays[attempt], true
}

func RetryBackoffDelayClamped(attempt int) time.Duration {
	switch {
	case len(retryBackoffDelays) == 0:
		return 0
	case attempt < 0:
		return retryBackoffDelays[0]
	case attempt >= len(retryBackoffDelays):
		return retryBackoffDelays[len(retryBackoffDelays)-1]
	default:
		return retryBackoffDelays[attempt]
	}
}

func RetryBackoffMaxAttempts() int {
	return len(retryBackoffDelays) + 1
}
