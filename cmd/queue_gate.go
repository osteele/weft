package cmd

import (
	"database/sql"
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

// validatePinnedHostQueueGate rejects queue submissions that are
// deterministically impossible for a pinned inventory host using local
// inventory data only.
func validatePinnedHostQueueGate(host string, constraints placement.Constraints) error {
	host = strings.TrimSpace(host)
	constraints.GPUClass = strings.TrimSpace(constraints.GPUClass)
	if host == "" || db.IsLaunchHost(host) {
		return nil
	}

	spec := inventory.FindHost(host)
	if spec == nil {
		return nil
	}

	if constraints.NeedsGPU() {
		if len(spec.GPUs) == 0 {
			return nil
		}
		knownClass := constraints.GPUClass == ""
		knownMem := constraints.GPUMemGB <= 0
		for _, gpu := range spec.GPUs {
			knownClass = knownClass || gpu.Class != "" || gpu.Name != ""
			knownMem = knownMem || inventory.ParseMemGB(gpu.Memory) > 0
		}
		if !knownClass || !knownMem {
			return nil
		}
	}

	if ok, reasons := placement.CheckHostGPUConstraints(*spec, constraints); !ok {
		if len(reasons) == 1 && constraints.GPUClass != "" && reasons[0] == fmt.Sprintf("no %s GPU", constraints.GPUClass) {
			return fmt.Errorf("gpu gate: no GPU matching class %s", constraints.GPUClass)
		}
		return fmt.Errorf("gpu gate: %s", strings.Join(reasons, "; "))
	}
	return nil
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

func blockedReasonDetailLines(database *sql.DB, job *db.Job, live string) []string {
	live = strings.TrimSpace(live)
	if live == "" {
		return nil
	}
	lines := []string{live}
	chain := queueblock.TraceWaitingOnProducer(database, job)
	for i := 1; i < len(chain); i++ {
		label := producerWaitDetailLabel("producer", chain[i])
		if i == len(chain)-1 {
			label = producerWaitDetailLabel("root", chain[i])
		}
		lines = append(lines, label+": "+chain[i].Reason())
	}
	return lines
}

func producerWaitDetailLabel(prefix string, wait queueblock.ProducerWait) string {
	if queueblock.ReasonKind(wait.Reason()) == queueblock.KindWaiting {
		return prefix + " wait"
	}
	return prefix + " blocker"
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
