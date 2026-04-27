package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/queueblock"
)

const queueBlockedReasonTimeout = 5 * time.Second

// validatePinnedHostQueueGate rejects queue submissions that are deterministically
// impossible for a pinned inventory host using local inventory data only.
func validatePinnedHostQueueGate(host, gpuClass string) error {
	host = strings.TrimSpace(host)
	gpuClass = strings.TrimSpace(gpuClass)
	if host == "" || gpuClass == "" || db.IsLaunchHost(host) {
		return nil
	}

	spec := inventory.FindHost(host)
	if spec == nil || len(spec.GPUs) == 0 {
		return nil
	}

	constraint := placement.ParseGPUConstraint(gpuClass)
	checkedAny := false
	for _, gpu := range spec.GPUs {
		if gpu.Class != "" {
			checkedAny = true
			if constraint.MatchesGPU(gpu.Class) {
				return nil
			}
		}
		if gpu.Name != "" {
			checkedAny = true
			if constraint.MatchesGPUFullName(gpu.Name) {
				return nil
			}
		}
	}
	if !checkedAny {
		return nil
	}
	return fmt.Errorf("gpu gate: no GPU matching class %s", gpuClass)
}

func hydrateQueueBlockedReasons(jobs []*db.Job) {
	queueblock.Apply(jobs, queueblock.Fetch(jobs, queueBlockedReasonTimeout))
	database, err := db.OpenForReading()
	if err != nil {
		return
	}
	defer database.Close()
	orchestration.HydrateUnplacedBlockedReasons(database, jobs)
}
