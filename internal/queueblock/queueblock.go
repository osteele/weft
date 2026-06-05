package queueblock

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/queuerunner"
)

// DisplayState captures the UI-only queue display state for a job.
type DisplayState struct {
	Status  string
	Reason  string
	Blocked bool
}

// Lookup maps host -> job id -> queue block reason.
type Lookup map[string]map[int64]string

var fetchQueueStatus = queuerunner.FetchStatus

// FromHosts builds a blocked-reason lookup from host queue metadata.
func FromHosts(hosts []*hostinfo.Host) Lookup {
	lookup := make(Lookup)
	for _, host := range hosts {
		if host == nil || host.Name == "" || len(host.BlockedQueueJobs) == 0 {
			continue
		}
		reasons := make(map[int64]string, len(host.BlockedQueueJobs))
		for jobID, reason := range host.BlockedQueueJobs {
			reason = strings.TrimSpace(reason)
			if reason != "" {
				reasons[jobID] = reason
			}
		}
		if len(reasons) > 0 {
			lookup[host.Name] = reasons
		}
	}
	return lookup
}

// Fetch queries hosts for blocked queued-job reasons for the provided jobs.
func Fetch(jobs []*db.Job, timeout time.Duration) Lookup {
	hosts := make(map[string]struct{})
	for _, job := range jobs {
		if job == nil || job.EffectiveStatus() != db.StatusQueued || !job.HasInventoryHost() {
			continue
		}
		hosts[job.Host] = struct{}{}
	}

	lookup := make(Lookup)
	for host := range hosts {
		status, err := fetchQueueStatus(host, timeout)
		if err != nil || status == nil || len(status.BlockedReasons) == 0 {
			continue
		}
		reasons := make(map[int64]string, len(status.BlockedReasons))
		for jobID, reason := range status.BlockedReasons {
			reason = strings.TrimSpace(reason)
			if reason != "" {
				reasons[jobID] = userVisibleBlockedReason(host, jobID, reason)
			}
		}
		if len(reasons) > 0 {
			lookup[host] = reasons
		}
	}
	return lookup
}

func userVisibleBlockedReason(host string, jobID int64, reason string) string {
	if !strings.Contains(reason, "missing queue payload") {
		return reason
	}
	bugID := reportMissingPayloadBug(host, jobID, reason)
	if bugID == "" {
		return "Weft bug: queue state is inconsistent on this host. Scope: infrastructure, not job-specific. Likelihood: uncommon; sync or runner restart usually repairs it."
	}
	return fmt.Sprintf("Weft bug %s: queue state is inconsistent on this host. Scope: infrastructure, not job-specific. Likelihood: uncommon; sync or runner restart usually repairs it.", bugID)
}

func reportMissingPayloadBug(host string, jobID int64, reason string) string {
	database, err := db.Open()
	if err != nil {
		return ""
	}
	defer database.Close()
	jobIDCopy := jobID
	bug, _, err := db.ReportBug(database, db.BugReport{
		Title:       "runner pending job is missing queue payload",
		Kind:        "invariant",
		Scope:       "infrastructure",
		Likelihood:  "uncommon; may persist for affected queued jobs until sync or runner restart repairs state",
		Severity:    "warning",
		Fingerprint: fmt.Sprintf("queue.missing_payload:%s", strings.TrimSpace(host)),
		JobID:       &jobIDCopy,
		Host:        host,
		Summary:     "Weft queue state is inconsistent on the remote host; this is not a job-code or input problem.",
		Detail:      reason,
	})
	if err != nil {
		return ""
	}
	return db.FormatBugID(bug.ID)
}

// Apply writes transient blocked-reason overlays onto the supplied jobs.
func Apply(jobs []*db.Job, lookup Lookup) {
	for _, job := range jobs {
		if job == nil {
			continue
		}
		job.QueueBlockedReason = ""
		if !isBlockable(job.EffectiveStatus()) {
			continue
		}
		job.QueueBlockedReason = reasonForJob(job, lookup)
	}
}

// Display returns the UI-only display state for a job.
func Display(job *db.Job, lookup Lookup) DisplayState {
	if job == nil {
		return DisplayState{}
	}
	status := job.EffectiveStatus()
	reason := strings.TrimSpace(job.QueueBlockedReason)
	if reason == "" {
		reason = reasonForJob(job, lookup)
	}
	if isBlockable(status) && reason != "" {
		return DisplayState{
			Status:  "blocked",
			Reason:  reason,
			Blocked: true,
		}
	}
	return DisplayState{
		Status: status,
		Reason: reason,
	}
}

// isBlockable reports whether a job in this effective status can carry a
// blocked-reason overlay. Both inventory-host queued jobs (StatusQueued with a
// host) and unplaced rental jobs (StatusQueued or StatusPendingPlacement with
// no host) qualify.
func isBlockable(status string) bool {
	return status == db.StatusQueued || status == db.StatusPendingPlacement
}

func reasonForJob(job *db.Job, lookup Lookup) string {
	if job == nil || lookup == nil || !job.HasInventoryHost() {
		return ""
	}
	reasons := lookup[job.Host]
	if len(reasons) == 0 {
		return ""
	}
	return strings.TrimSpace(reasons[job.ID])
}
