package campaign

import (
	"fmt"
	"sort"
	"strings"
)

type LaunchEventKind string

const (
	LaunchEventCampaignStatus LaunchEventKind = "campaign_status"
	LaunchEventGroupPhase     LaunchEventKind = "group_phase"
	LaunchEventGroupAssets    LaunchEventKind = "group_assets"
	LaunchEventGroupRetry     LaunchEventKind = "group_retry"
	// LaunchEventGroupReplan is emitted when the per-group goroutine
	// abandons an exhausted create-with-replacement chain and starts a
	// fresh chain against a newly searched initial offer. Distinct from
	// LaunchEventGroupRetry (which fires for in-chain replacement
	// attempts) so the TUI can label them differently — one is "another
	// offer on the same chain," the other is "the whole chain is being
	// restarted."
	LaunchEventGroupReplan LaunchEventKind = "group_replan"
	LaunchEventGroupDone   LaunchEventKind = "group_done"
	LaunchEventGroupFailed LaunchEventKind = "group_failed"
)

type LaunchEvent struct {
	Kind LaunchEventKind

	Group InstanceGroup

	Phase string

	AssetsReady int
	AssetsTotal int

	RetryAttempt int
	RetryMax     int

	InstanceID int64
	Reason     string
}

func LaunchGroupSignature(group InstanceGroup) string {
	jobIDs := make([]int64, 0, len(group.Jobs))
	for _, job := range group.Jobs {
		if job == nil || job.ID <= 0 {
			continue
		}
		jobIDs = append(jobIDs, job.ID)
	}
	sort.Slice(jobIDs, func(i, j int) bool { return jobIDs[i] < jobIDs[j] })

	var b strings.Builder
	b.WriteString(strings.TrimSpace(group.GPUSpec()))
	b.WriteString("|")
	for i, id := range jobIDs {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(fmt.Sprintf("%d", id))
	}
	return b.String()
}
