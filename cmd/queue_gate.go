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

// mergeBlockedReasons returns the live blocked reason followed by any
// additional historical placement reasons, deduplicated and trimmed. A
// previously-placed job can carry both a current block (e.g. waiting on a
// producer) and a record of why its prior placement no longer applies (e.g.
// "cloud instance N unavailable") — both are useful context.
func mergeBlockedReasons(live string, placement []string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(reason string) {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return
		}
		if _, dup := seen[reason]; dup {
			return
		}
		seen[reason] = struct{}{}
		out = append(out, reason)
	}
	add(live)
	for _, r := range placement {
		add(r)
	}
	return out
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
