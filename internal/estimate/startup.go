package estimate

import "time"

// EstimateStartup returns the estimated time for a cloud instance to boot and
// become reachable via SSH. The provider parameter is reserved for future
// per-provider tuning.
func EstimateStartup(_ string) Estimate {
	return Estimate{
		Mean:  45 * time.Second,
		Lower: 30 * time.Second,
		Upper: 3 * time.Minute,
	}
}
