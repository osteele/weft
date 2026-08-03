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

	verdict := placement.CheckHostConstraints(*spec, constraints)
	if verdict.Eligible {
		return nil
	}
	reasons := verdict.Reasons
	if constraints.NeedsGPU() && hostGPUInventoryIncomplete(*spec, constraints) {
		reasons = filterEligibilityReasons(reasons, func(reason placement.EligibilityReason) bool {
			return !isGPUInventoryReason(reason.Kind)
		})
		if len(reasons) == 0 {
			return nil
		}
	}

	if len(reasons) == 1 {
		if constraints.GPUClass != "" && reasons[0].Message == fmt.Sprintf("no %s GPU", constraints.GPUClass) {
			return fmt.Errorf("gpu gate: no GPU matching class %s", constraints.GPUClass)
		}
	}
	return fmt.Errorf("%s gate: %s", queueGateAxis(reasons), strings.Join(eligibilityMessages(reasons), "; "))
}

func hostGPUInventoryIncomplete(host inventory.HostSpec, constraints placement.Constraints) bool {
	if len(host.GPUs) == 0 {
		return true
	}
	knownClass := constraints.GPUClass == ""
	knownMem := constraints.GPUMemGB <= 0
	for _, gpu := range host.GPUs {
		knownClass = knownClass || gpu.Class != "" || gpu.Name != ""
		knownMem = knownMem || inventory.ParseMemGB(gpu.Memory) > 0
	}
	return !knownClass || !knownMem
}

func filterEligibilityReasons(reasons []placement.EligibilityReason, keep func(placement.EligibilityReason) bool) []placement.EligibilityReason {
	out := make([]placement.EligibilityReason, 0, len(reasons))
	for _, reason := range reasons {
		if keep(reason) {
			out = append(out, reason)
		}
	}
	return out
}

func isGPUInventoryReason(kind placement.EligibilityReasonKind) bool {
	switch kind {
	case placement.ReasonGPUClass, placement.ReasonGPUMemory, placement.ReasonGPUCount,
		placement.ReasonGPUAvailability, placement.ReasonComputeCapMax, placement.ReasonComputeCapMin:
		return true
	default:
		return false
	}
}

func queueGateAxis(reasons []placement.EligibilityReason) string {
	if len(reasons) == 0 {
		return "host"
	}
	switch reasons[0].Kind {
	case placement.ReasonGPUClass, placement.ReasonGPUMemory, placement.ReasonGPUCount,
		placement.ReasonGPUAvailability, placement.ReasonComputeCapMax, placement.ReasonComputeCapMin:
		return "gpu"
	case placement.ReasonCPUCores:
		return "cpu"
	case placement.ReasonHostRAM:
		return "memory"
	case placement.ReasonInterconnect:
		return "interconnect"
	default:
		return "host"
	}
}

func eligibilityMessages(reasons []placement.EligibilityReason) []string {
	msgs := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		msgs = append(msgs, reason.Message)
	}
	return msgs
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
