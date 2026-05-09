package narrate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/progress"
)

type formattedJobOut struct {
	ID                 int64    `json:"id"`
	Status             string   `json:"status"`
	Host               string   `json:"host,omitempty"`
	Project            string   `json:"project,omitempty"`
	LaunchID           *int64   `json:"instance_id,omitempty"`
	Tags               string   `json:"tags,omitempty"`
	Command            string   `json:"command,omitempty"`
	AgeSeconds         int64    `json:"age_seconds,omitempty"`
	ExitCode           *int     `json:"exit_code,omitempty"`
	PlacementReasons   []string `json:"placement_blocked_reasons,omitempty"`
	QueueBlockedReason string   `json:"queue_blocked_reason,omitempty"`
	FailureReason      string   `json:"failure_reason,omitempty"`
	ErrorMessage       string   `json:"error_message,omitempty"`
	Progress           string   `json:"progress,omitempty"`
}

// FormatSnapshot renders a snapshot as compact JSON for the model. We keep
// it deterministic (sorted maps via json.Marshal of structs with maps is not
// guaranteed stable, so we project to slices) and trimmed.
func FormatSnapshot(s *Snapshot) string {
	if s == nil {
		return "{}"
	}
	type instOut struct {
		ID                int64   `json:"id"`
		CampaignID        *int64  `json:"campaign_id,omitempty"`
		Status            string  `json:"status"`
		Provider          string  `json:"provider,omitempty"`
		GPU               string  `json:"gpu,omitempty"`
		NumGPUs           int     `json:"num_gpus,omitempty"`
		AgeSeconds        int64   `json:"age_seconds,omitempty"`
		CostPerHourUSD    float64 `json:"cost_per_hr_usd,omitempty"`
		GraceDeadlineSecs *int64  `json:"grace_deadline_unix,omitempty"`
		Cordoned          bool    `json:"cordoned,omitempty"`
		TerminationReason string  `json:"termination_reason,omitempty"`
	}
	type autopilotOut struct {
		State        string `json:"state,omitempty"`
		Paused       bool   `json:"paused,omitempty"`
		PausedReason string `json:"paused_reason,omitempty"`
		LastError    string `json:"last_error,omitempty"`
	}
	now := s.Time
	if now.IsZero() {
		now = time.Now()
	}
	out := struct {
		At        string            `json:"at"`
		Autopilot *autopilotOut     `json:"autopilot,omitempty"`
		Instances []instOut         `json:"instances,omitempty"`
		Jobs      []formattedJobOut `json:"jobs,omitempty"`
	}{
		At: now.Format(time.RFC3339),
	}
	if autopilotNeedsSnapshotContext(s.Autopilot) {
		out.Autopilot = &autopilotOut{
			State:        s.Autopilot.State,
			Paused:       s.Autopilot.Paused,
			PausedReason: s.Autopilot.PausedReason,
			LastError:    s.Autopilot.LastError,
		}
	}
	for _, inst := range orderedInstances(s.Instances) {
		out.Instances = append(out.Instances, instOut{
			ID:                inst.ID,
			CampaignID:        inst.CampaignID,
			Status:            inst.Status,
			Provider:          inst.Provider,
			GPU:               inst.GPUSpec,
			NumGPUs:           inst.NumGPUs,
			AgeSeconds:        ageSeconds(inst.CreatedAt, now),
			CostPerHourUSD:    centsToUSD(inst.CostPerHourCents),
			GraceDeadlineSecs: inst.GraceDeadline,
			Cordoned:          inst.Cordoned,
			TerminationReason: inst.TerminationReason,
		})
	}
	for _, j := range orderedJobs(s.Jobs) {
		out.Jobs = append(out.Jobs, jobToOut(j, now))
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// FormatDelta renders the transitions since the previous tick.
func FormatDelta(d Delta) string {
	type jobChange struct {
		ID           int64  `json:"id"`
		Project      string `json:"project,omitempty"`
		From         string `json:"from"`
		To           string `json:"to"`
		Host         string `json:"host,omitempty"`
		Exit         *int   `json:"exit_code,omitempty"`
		PrevLaunchID *int64 `json:"prev_instance_id,omitempty"`
		Note         string `json:"note,omitempty"`
	}
	type instChange struct {
		ID                int64  `json:"id"`
		From              string `json:"from"`
		To                string `json:"to"`
		TerminationReason string `json:"termination_reason,omitempty"`
		Note              string `json:"note,omitempty"`
	}
	type instTermOut struct {
		ID                int64  `json:"id"`
		FromStatus        string `json:"from"`
		TerminationReason string `json:"termination_reason,omitempty"`
		TerminationDetail string `json:"termination_detail,omitempty"`
		Provider          string `json:"provider,omitempty"`
		GPU               string `json:"gpu,omitempty"`
	}
	type finishedOut struct {
		ID            int64  `json:"id"`
		Project       string `json:"project,omitempty"`
		Status        string `json:"status"`
		ExitCode      *int   `json:"exit_code,omitempty"`
		FailureReason string `json:"failure_reason,omitempty"`
		ErrorMessage  string `json:"error_message,omitempty"`
	}
	out := struct {
		JobsAdded      []formattedJobOut `json:"jobs_added,omitempty"`
		JobsChanged    []jobChange       `json:"jobs_changed,omitempty"`
		JobsFinished   []finishedOut     `json:"jobs_finished,omitempty"`
		InstAdded      []InstanceView    `json:"instances_added,omitempty"`
		InstChanged    []instChange      `json:"instances_changed,omitempty"`
		InstTerminated []instTermOut     `json:"instances_terminated,omitempty"`
		AutopilotFrom  string            `json:"autopilot_from,omitempty"`
		AutopilotTo    string            `json:"autopilot_to,omitempty"`
	}{
		InstAdded: d.InstAdded,
	}
	now := time.Now()
	for _, j := range d.JobAdded {
		out.JobsAdded = append(out.JobsAdded, jobToOut(j, now))
	}
	for _, inst := range d.InstTerminated {
		out.InstTerminated = append(out.InstTerminated, instTermOut{
			ID:                inst.ID,
			FromStatus:        inst.Status,
			TerminationReason: inst.TerminationReason,
			TerminationDetail: inst.TerminationDetail,
			Provider:          inst.Provider,
			GPU:               inst.GPUSpec,
		})
	}
	for _, j := range d.JobFinished {
		out.JobsFinished = append(out.JobsFinished, finishedOut{
			ID:            j.ID,
			Project:       j.Project,
			Status:        j.Status,
			ExitCode:      j.ExitCode,
			FailureReason: j.FailureReason,
			ErrorMessage:  j.ErrorMessage,
		})
	}
	for _, c := range d.JobChanged {
		jc := jobChange{
			ID:      c.After.ID,
			Project: c.After.Project,
			From:    c.Before.Status,
			To:      c.After.Status,
			Host:    c.After.Host,
			Exit:    c.After.ExitCode,
		}
		// Surface the instance the job was on before the transition. When
		// after.LaunchID is nil (job got requeued), the model can correlate
		// against instances_terminated for the causal link.
		if c.Before.LaunchID != nil && (c.After.LaunchID == nil || *c.Before.LaunchID != *c.After.LaunchID) {
			id := *c.Before.LaunchID
			jc.PrevLaunchID = &id
		}
		if c.Before.Host != c.After.Host {
			jc.Note = "host changed: " + c.Before.Host + " -> " + c.After.Host
		}
		out.JobsChanged = append(out.JobsChanged, jc)
	}
	for _, c := range d.InstChanged {
		ic := instChange{ID: c.After.ID, From: c.Before.Status, To: c.After.Status, TerminationReason: c.After.TerminationReason}
		if c.Before.Cordoned != c.After.Cordoned {
			if c.After.Cordoned {
				ic.Note = "cordoned"
			} else {
				ic.Note = "uncordoned"
			}
		}
		out.InstChanged = append(out.InstChanged, ic)
	}
	if autopilotChangeNeedsNarration(d.AutopilotOld, d.AutopilotNew) {
		out.AutopilotFrom = d.AutopilotOld.State
		out.AutopilotTo = d.AutopilotNew.State
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// FormatLifecycleEvents renders durable DB lifecycle events as compact JSON for
// the narrator. The input is expected oldest-first, as returned by
// db.ListLifecycleEventsAfterID.
func FormatLifecycleEvents(events []db.LifecycleEvent) string {
	type eventOut struct {
		ID            int64  `json:"id"`
		At            string `json:"at"`
		Kind          string `json:"kind"`
		LaunchID      int64  `json:"instance_id,omitempty"`
		CampaignID    int64  `json:"campaign_id,omitempty"`
		JobID         int64  `json:"job_id,omitempty"`
		GPUSpec       string `json:"gpu,omitempty"`
		JobCount      int    `json:"job_count,omitempty"`
		Detail        string `json:"detail,omitempty"`
		ErrorText     string `json:"error,omitempty"`
		AttemptNumber int    `json:"attempt,omitempty"`
		MaxAttempts   int    `json:"max_attempts,omitempty"`
		DiskGB        int    `json:"disk_gb,omitempty"`
	}
	out := struct {
		Events []eventOut `json:"events,omitempty"`
	}{}
	for _, event := range events {
		out.Events = append(out.Events, eventOut{
			ID:            event.ID,
			At:            time.Unix(event.OccurredAt, 0).Format(time.RFC3339),
			Kind:          event.EventKind,
			LaunchID:      event.LaunchID,
			CampaignID:    event.CampaignID,
			JobID:         event.JobID,
			GPUSpec:       event.GPUSpec,
			JobCount:      event.JobCount,
			Detail:        event.Detail,
			ErrorText:     event.ErrorText,
			AttemptNumber: event.AttemptNumber,
			MaxAttempts:   event.MaxAttempts,
			DiskGB:        event.DiskGB,
		})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// FallbackNarration returns a compact local summary when the LLM call fails or
// returns an empty narration. It is intentionally factual and low-flair.
func FallbackNarration(d Delta) string {
	parts := make([]string, 0, 6)
	if n := len(d.JobAdded); n > 0 {
		parts = append(parts, fmt.Sprintf("%d job%s entered the active queue", n, plural(n)))
	}
	if n := len(d.JobChanged); n > 0 {
		parts = append(parts, fmt.Sprintf("%d job%s changed state", n, plural(n)))
	}
	if n := len(d.JobFinished); n > 0 {
		completed, failed := terminalJobCounts(d.JobFinished)
		switch {
		case completed > 0 && failed > 0:
			parts = append(parts, fmt.Sprintf("%d job%s finished: %d completed, %d failed", n, plural(n), completed, failed))
		case completed > 0:
			parts = append(parts, fmt.Sprintf("%d job%s completed", completed, plural(completed)))
		case failed > 0:
			parts = append(parts, fmt.Sprintf("%d job%s failed or stopped", failed, plural(failed)))
		default:
			parts = append(parts, fmt.Sprintf("%d job%s finished", n, plural(n)))
		}
	}
	if n := len(d.InstAdded); n > 0 {
		parts = append(parts, fmt.Sprintf("%d instance%s appeared", n, plural(n)))
	}
	if n := len(d.InstChanged); n > 0 {
		parts = append(parts, fmt.Sprintf("%d instance%s changed state", n, plural(n)))
	}
	if n := len(d.InstTerminated); n > 0 {
		parts = append(parts, fmt.Sprintf("%d instance%s terminated", n, plural(n)))
	}
	if autopilotChangeNeedsNarration(d.AutopilotOld, d.AutopilotNew) {
		parts = append(parts, fmt.Sprintf("autopilot moved from %s to %s", d.AutopilotOld.State, d.AutopilotNew.State))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + "."
}

func FallbackEventNarration(events []db.LifecycleEvent) string {
	if len(events) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, event := range events {
		counts[event.EventKind]++
	}
	kinds := make([]string, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	parts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		n := counts[kind]
		parts = append(parts, fmt.Sprintf("%d %s event%s", n, kind, plural(n)))
	}
	return strings.Join(parts, "; ") + "."
}

func FallbackStatusNarration(sl StatusLine) string {
	parts := make([]string, 0, 6)
	if sl.ActiveInstances > 0 || sl.LaunchingInst > 0 || sl.GraceInstances > 0 {
		var inst []string
		if sl.ActiveInstances > 0 {
			inst = append(inst, fmt.Sprintf("%d running", sl.ActiveInstances))
		}
		if sl.LaunchingInst > 0 {
			inst = append(inst, fmt.Sprintf("%d launching", sl.LaunchingInst))
		}
		if sl.GraceInstances > 0 {
			inst = append(inst, fmt.Sprintf("%d in grace", sl.GraceInstances))
		}
		parts = append(parts, "instances: "+strings.Join(inst, ", "))
	} else {
		parts = append(parts, "no rentals")
	}
	jobParts := make([]string, 0, 4)
	if sl.RunningJobs > 0 {
		jobParts = append(jobParts, fmt.Sprintf("%d running", sl.RunningJobs))
	}
	if sl.QueuedJobs > 0 {
		jobParts = append(jobParts, fmt.Sprintf("%d queued", sl.QueuedJobs))
	}
	if sl.StartingJobs > 0 {
		jobParts = append(jobParts, fmt.Sprintf("%d starting", sl.StartingJobs))
	}
	if sl.PendingPlacement > 0 {
		jobParts = append(jobParts, fmt.Sprintf("%d pending placement", sl.PendingPlacement))
	}
	if len(jobParts) > 0 {
		parts = append(parts, "jobs: "+strings.Join(jobParts, ", "))
	}
	if sl.UnprocessedCompleted > 0 || sl.UnprocessedFailed > 0 {
		parts = append(parts, fmt.Sprintf("unprocessed: %d completed, %d failed", sl.UnprocessedCompleted, sl.UnprocessedFailed))
	}
	if sl.AutopilotState != "" {
		parts = append(parts, "autopilot "+sl.AutopilotState)
	}
	return "Current status: " + strings.Join(parts, "; ") + "."
}

func terminalJobCounts(jobs []JobView) (completed, failed int) {
	for _, job := range jobs {
		if job.Status == "completed" && (job.ExitCode == nil || *job.ExitCode == 0) {
			completed++
			continue
		}
		failed++
	}
	return completed, failed
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func autopilotNeedsSnapshotContext(v AutopilotView) bool {
	return v.Paused || v.State == "paused" || v.State == "stale" || strings.TrimSpace(v.LastError) != ""
}

func jobToOut(j JobView, now time.Time) formattedJobOut {
	out := formattedJobOut{
		ID:                 j.ID,
		Status:             j.Status,
		Host:               j.Host,
		Project:            j.Project,
		LaunchID:           j.LaunchID,
		Tags:               strings.Join(j.Tags, ","),
		Command:            j.Command,
		AgeSeconds:         ageSeconds(j.StartTime, now),
		ExitCode:           j.ExitCode,
		QueueBlockedReason: j.QueueBlockedReason,
		FailureReason:      j.FailureReason,
		ErrorMessage:       j.ErrorMessage,
	}
	if j.ProgressPct >= 0 {
		out.Progress = strings.TrimSpace(progress.FormatPhaseProgress(j.ProgressPhase, j.ProgressPct))
	}
	if jobPlacementReasonsAreCurrent(j) {
		out.PlacementReasons = j.PlacementReasons
	}
	return out
}

func jobPlacementReasonsAreCurrent(j JobView) bool {
	if len(j.PlacementReasons) == 0 {
		return false
	}
	return j.Status == "pending_placement" || (j.Status == "queued" && j.Host == "" && j.LaunchID == nil)
}

// FormatPriorRecap concatenates accumulated recaps into one text block.
func FormatPriorRecap(entries []recapEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, e := range entries {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString("[t=")
		sb.WriteString(e.At.UTC().Format(time.RFC3339))
		sb.WriteString("]\n")
		sb.WriteString(e.Recap)
	}
	return sb.String()
}

func orderedJobs(m map[int64]JobView) []JobView {
	out := make([]JobView, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func orderedInstances(m map[int64]InstanceView) []InstanceView {
	out := make([]InstanceView, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func ageSeconds(unix int64, now time.Time) int64 {
	if unix <= 0 {
		return 0
	}
	d := now.Unix() - unix
	if d < 0 {
		return 0
	}
	return d
}

func centsToUSD(cents int) float64 {
	if cents == 0 {
		return 0
	}
	return float64(cents) / 100.0
}
