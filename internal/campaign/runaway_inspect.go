package campaign

import (
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
)

// RunawayBreakerInfo summarises why a scope is currently paused by the
// runaway breaker: the trip event's metrics (chain length, orphan count,
// spend, window) and timestamp, plus the scope keys (campaign_id and
// project) that were tripped. Operators get a structured answer to
// "why is wj_X blocked" instead of the canned "paused: repeated launch
// failures without progress" string.
//
// Produced by LookupActiveRunawayBreakers, which joins each scope's
// latest trip event against its latest resume event and surfaces only
// the scopes where trip > resume.
type RunawayBreakerInfo struct {
	CampaignID int64         // 0 == "no campaign linkage" (global scope)
	Project    string        // "<all>" when the trip wasn't project-scoped
	Reason     string        // stable machine-readable reason for the trip
	TrippedAt  time.Time     // when the trip event was recorded
	Chain      int           // longest trailing-orphaned chain at trip time
	Orphaned   int           // total orphaned attempts in window at trip time
	InfraFails int           // total infra-side failures in window at trip time
	SpendCents int           // dollars spent without progress, in cents
	Window     time.Duration // policy window used at trip time
	RawDetail  string        // verbatim lifecycle_events.detail (for debugging)
}

const (
	RunawayBreakerReasonLaunchFailures = "repeated_launch_failures_without_progress"
	RunawayBreakerReasonInfraFailures  = "repeated_infrastructure_failures_without_progress"
)

// MetricsLine renders the structured fields the way they appear in the
// stored detail string, e.g. "chain=2 orphaned=8 spend=$0.41 window=24h".
func (i RunawayBreakerInfo) MetricsLine() string {
	w := i.Window.String()
	if i.Window <= 0 {
		w = "?"
	}
	if i.InfraFails > 0 {
		return fmt.Sprintf("chain=%d orphaned=%d infra_failures=%d spend=$%.2f window=%s",
			i.Chain, i.Orphaned, i.InfraFails, float64(i.SpendCents)/100.0, w)
	}
	return fmt.Sprintf("chain=%d orphaned=%d spend=$%.2f window=%s",
		i.Chain, i.Orphaned, float64(i.SpendCents)/100.0, w)
}

// ScopeLabel renders the scope as it would appear in operator output.
// e.g. "campaign 273, project=role-encoding-injection" or
// "global (project=<all>)".
func (i RunawayBreakerInfo) ScopeLabel() string {
	parts := []string{}
	if i.CampaignID > 0 {
		parts = append(parts, fmt.Sprintf("campaign %d", i.CampaignID))
	}
	parts = append(parts, "project="+i.Project)
	if i.CampaignID == 0 {
		return "global (" + parts[0] + ")"
	}
	return strings.Join(parts, ", ")
}

// LookupActiveRunawayBreakers returns one RunawayBreakerInfo per scope
// that is currently tripped: the scope's latest trip event is more
// recent than its latest resume event. Scopes that have been resumed
// (or never tripped) are omitted.
//
// "Scope" is the (campaign_id, project) tuple as recorded in the
// lifecycle_events rows. The lookup reads bounded sets of trip and resume
// events and reconstructs the matching map in Go because the project portion
// is encoded in the detail string.
func LookupActiveRunawayBreakers(database *sql.DB) ([]RunawayBreakerInfo, error) {
	if database == nil {
		return nil, nil
	}
	var events []db.LifecycleEvent
	for _, kind := range []string{db.EventRelaunchRunawayTripped, db.EventRelaunchRunawayResumed} {
		rows, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{
			Kind:  kind,
			Limit: 1000,
		})
		if err != nil {
			return nil, fmt.Errorf("list runaway events: %w", err)
		}
		events = append(events, rows...)
	}

	type scopeKey struct {
		CampaignID int64
		Project    string
	}
	type scopeState struct {
		trip   *db.LifecycleEvent
		resume *db.LifecycleEvent
	}
	state := make(map[scopeKey]*scopeState)
	for i := range events {
		e := events[i]
		if e.EventKind != db.EventRelaunchRunawayTripped &&
			e.EventKind != db.EventRelaunchRunawayResumed {
			continue
		}
		project := runawayProjectLabel(runawayProjectFromDetail(e.Detail))
		k := scopeKey{CampaignID: e.CampaignID, Project: project}
		s, ok := state[k]
		if !ok {
			s = &scopeState{}
			state[k] = s
		}
		switch e.EventKind {
		case db.EventRelaunchRunawayTripped:
			if s.trip == nil || e.OccurredAt > s.trip.OccurredAt {
				ev := e
				s.trip = &ev
			}
		case db.EventRelaunchRunawayResumed:
			if s.resume == nil || e.OccurredAt > s.resume.OccurredAt {
				ev := e
				s.resume = &ev
			}
		}
	}

	// "Global" resume events (CampaignID=0, project=<all>) override every
	// more-specific scope: they are the reset operator action. Find the
	// latest such resume and treat any earlier trip as cleared.
	var globalResumeAt int64
	for k, s := range state {
		if k.CampaignID == 0 && k.Project == "<all>" && s.resume != nil {
			globalResumeAt = s.resume.OccurredAt
			break
		}
	}

	var out []RunawayBreakerInfo
	for k, s := range state {
		if s.trip == nil {
			continue
		}
		latestResume := int64(0)
		if s.resume != nil {
			latestResume = s.resume.OccurredAt
		}
		if globalResumeAt > latestResume {
			latestResume = globalResumeAt
		}
		if s.trip.OccurredAt <= latestResume {
			continue
		}
		info := RunawayBreakerInfo{
			CampaignID: k.CampaignID,
			Project:    k.Project,
			Reason:     RunawayBreakerReasonLaunchFailures,
			TrippedAt:  time.Unix(s.trip.OccurredAt, 0),
			RawDetail:  s.trip.Detail,
		}
		if strings.Contains(s.trip.Detail, "infra runaway:") {
			info.Reason = RunawayBreakerReasonInfraFailures
		}
		parseRunawayMetrics(s.trip.Detail, &info)
		out = append(out, info)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].TrippedAt.Equal(out[j].TrippedAt) {
			return out[i].CampaignID < out[j].CampaignID
		}
		return out[i].TrippedAt.After(out[j].TrippedAt)
	})
	return out, nil
}

// LookupRunawayBreakerForJob returns the active trip info for the scope
// covering a particular job, if one exists. The job's project and
// inferred campaign are used as the scope keys.
func LookupRunawayBreakerForJob(database *sql.DB, job *db.Job) (*RunawayBreakerInfo, error) {
	if database == nil || job == nil {
		return nil, nil
	}
	all, err := LookupActiveRunawayBreakers(database)
	if err != nil {
		return nil, err
	}
	wantProject := runawayProjectLabel(job.Project)
	wantCampaign := inferScopeCampaignID(database, []*db.Job{job})
	for i := range all {
		info := all[i]
		// Match scope: the trip's project must equal the job's project
		// (or be the wildcard "<all>"), and the trip's campaign_id must
		// equal the job's inferred campaign (or be the global 0).
		if info.Project != "<all>" && info.Project != wantProject {
			continue
		}
		if info.CampaignID != 0 && info.CampaignID != wantCampaign {
			continue
		}
		return &info, nil
	}
	return nil, nil
}

// JobsBlockedByBreaker returns the public IDs of currently-queued jobs
// that share the breaker's scope. Used by `weft autopilot blocked` to
// list the affected jobs alongside each tripped scope.
func JobsBlockedByBreaker(database *sql.DB, info RunawayBreakerInfo) ([]int64, error) {
	if database == nil {
		return nil, nil
	}
	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case info.Project != "<all>":
		rows, err = database.Query(`
			SELECT j.id FROM jobs j
			LEFT JOIN job_status js ON js.id = j.id
			WHERE js.status = 'queued'
			  AND COALESCE(j.project, '') = ?
			  AND j.tombstoned = 0
			ORDER BY j.id
		`, info.Project)
	default:
		rows, err = database.Query(`
			SELECT j.id FROM jobs j
			LEFT JOIN job_status js ON js.id = j.id
			WHERE js.status = 'queued' AND j.tombstoned = 0
			ORDER BY j.id
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// parseRunawayMetrics fills the metric fields on info from a stored
// detail string like "project=<all>; no-progress runaway: chain=2
// orphaned=8 spend=$0.41 window=24h0m0s". Unparseable fields stay zero.
func parseRunawayMetrics(detail string, info *RunawayBreakerInfo) {
	if m := chainRegexp.FindStringSubmatch(detail); len(m) == 2 {
		if v, err := strconv.Atoi(m[1]); err == nil {
			info.Chain = v
		}
	}
	if m := orphanedRegexp.FindStringSubmatch(detail); len(m) == 2 {
		if v, err := strconv.Atoi(m[1]); err == nil {
			info.Orphaned = v
		}
	}
	if m := infraFailuresRegexp.FindStringSubmatch(detail); len(m) == 2 {
		if v, err := strconv.Atoi(m[1]); err == nil {
			info.InfraFails = v
		}
	}
	if m := spendRegexp.FindStringSubmatch(detail); len(m) == 2 {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			info.SpendCents = int(v*100 + 0.5)
		}
	}
	if m := windowRegexp.FindStringSubmatch(detail); len(m) == 2 {
		if d, err := time.ParseDuration(m[1]); err == nil {
			info.Window = d
		}
	}
}

var (
	chainRegexp         = regexp.MustCompile(`chain=(\d+)`)
	orphanedRegexp      = regexp.MustCompile(`orphaned=(\d+)`)
	infraFailuresRegexp = regexp.MustCompile(`infra_failures=(\d+)`)
	spendRegexp         = regexp.MustCompile(`spend=\$([\d.]+)`)
	windowRegexp        = regexp.MustCompile(`window=([0-9smhd]+)`)
)
