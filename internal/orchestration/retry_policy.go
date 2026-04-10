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

func RetryBackoffMaxAttempts() int {
	return len(retryBackoffDelays) + 1
}
