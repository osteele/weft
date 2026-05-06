package narrate

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

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
	type jobOut struct {
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
	}
	now := s.Time
	if now.IsZero() {
		now = time.Now()
	}
	out := struct {
		At        string    `json:"at"`
		Autopilot any       `json:"autopilot"`
		Instances []instOut `json:"instances,omitempty"`
		Jobs      []jobOut  `json:"jobs,omitempty"`
	}{
		At: now.Format(time.RFC3339),
		Autopilot: map[string]any{
			"state":         s.Autopilot.State,
			"paused":        s.Autopilot.Paused,
			"paused_reason": s.Autopilot.PausedReason,
			"pass_age_s":    s.Autopilot.PassAgeSeconds,
			"last_summary":  s.Autopilot.LastSummary,
			"last_error":    s.Autopilot.LastError,
		},
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
		out.Jobs = append(out.Jobs, jobOut{
			ID:                 j.ID,
			Status:             j.Status,
			Host:               j.Host,
			Project:            j.Project,
			LaunchID:           j.LaunchID,
			Tags:               strings.Join(j.Tags, ","),
			Command:            j.Command,
			AgeSeconds:         ageSeconds(j.StartTime, now),
			ExitCode:           j.ExitCode,
			PlacementReasons:   j.PlacementReasons,
			QueueBlockedReason: j.QueueBlockedReason,
			FailureReason:      j.FailureReason,
			ErrorMessage:       j.ErrorMessage,
		})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// FormatDelta renders the transitions since the previous tick.
func FormatDelta(d Delta) string {
	type jobChange struct {
		ID   int64  `json:"id"`
		From string `json:"from"`
		To   string `json:"to"`
		Host string `json:"host,omitempty"`
		Exit *int   `json:"exit_code,omitempty"`
		Note string `json:"note,omitempty"`
	}
	type instChange struct {
		ID                int64  `json:"id"`
		From              string `json:"from"`
		To                string `json:"to"`
		TerminationReason string `json:"termination_reason,omitempty"`
		Note              string `json:"note,omitempty"`
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
		JobsAdded     []JobView      `json:"jobs_added,omitempty"`
		JobsChanged   []jobChange    `json:"jobs_changed,omitempty"`
		JobsFinished  []finishedOut  `json:"jobs_finished,omitempty"`
		InstAdded     []InstanceView `json:"instances_added,omitempty"`
		InstChanged   []instChange   `json:"instances_changed,omitempty"`
		AutopilotFrom string         `json:"autopilot_from,omitempty"`
		AutopilotTo   string         `json:"autopilot_to,omitempty"`
	}{
		JobsAdded: d.JobAdded,
		InstAdded: d.InstAdded,
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
		jc := jobChange{ID: c.After.ID, From: c.Before.Status, To: c.After.Status, Host: c.After.Host, Exit: c.After.ExitCode}
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
	if d.AutopilotOld.State != d.AutopilotNew.State {
		out.AutopilotFrom = d.AutopilotOld.State
		out.AutopilotTo = d.AutopilotNew.State
	}
	b, _ := json.Marshal(out)
	return string(b)
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
