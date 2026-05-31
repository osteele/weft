package dashtabs

import (
	"database/sql"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/jobeta"
	"github.com/osteele/weft/internal/llmusage"
	"github.com/osteele/weft/internal/logging"
)

// Snapshot is the data fed into every view on each refresh tick. Views never
// query the DB directly; they read from the Snapshot the parent passes in.
type Snapshot struct {
	LoadedAt        time.Time
	Jobs            []*db.Job // active jobs (running, queued, recently terminal)
	Hosts           []*hostinfo.Host
	LaunchLiveByID  map[int64]*db.LaunchLiveState // live progress per launch (for ETA)
	LiveInstances   []*db.Launch                  // actually live (running + transient setup)
	StaleInstances  []*db.Launch                  // non-terminal but not live (e.g. old "planned")
	RecentInstances []*db.Launch                  // terminated within the last 24h (newest first)
	AutopilotState  *db.AutopilotState
	SpendUSDPerHour float64 // sum across LiveInstances only
	SpendTargetUSD  float64
	Counts          StatusCounts
	RecentFailures  []FailureSummary
	History         RecentHistory
	ProjectSpend24h []db.ProjectSpend // last 24h, sorted by spend desc
	ProjectSpend7d  []db.ProjectSpend // last 7 days, for context
	LLMUsage24h     []llmusage.FeatureUsage
	LLMUsage7d      []llmusage.FeatureUsage
	LLMDailySpend   []llmusage.DaySpend // last 30 days, newest first
}

// CloudInstances returns the union of LiveInstances and StaleInstances. Views
// that don't care about the distinction (e.g. History) use this; Fleet uses
// LiveInstances directly.
func (s *Snapshot) CloudInstances() []*db.Launch {
	out := make([]*db.Launch, 0, len(s.LiveInstances)+len(s.StaleInstances))
	out = append(out, s.LiveInstances...)
	out = append(out, s.StaleInstances...)
	return out
}

// StatusCounts derives running/queued/done/failed/etc. from the Jobs slice.
type StatusCounts struct {
	Running   int
	Queued    int
	Completed int
	Failed    int
	Killed    int
	Other     int
}

// FailureSummary describes a single recent failed instance grouped by provider.
type FailureSummary struct {
	Provider string
	Count    int
	When     time.Time
	Note     string
}

// RecentHistory is the rolling-window data the parent accumulates across
// refresh ticks. Each value is the most-recent-first sample.
type RecentHistory struct {
	Times      []time.Time
	QueueDepth []int
	RunningCnt []int
	SpendPerHr []float64
	FailureCnt []int
}

const HistoryLen = 60

// liveLaunchStatuses are the launch statuses we treat as "actually live" —
// the instance has been (or is being) created on the provider and may be
// incurring cost. We exclude "planned" because those are intents whose
// provider instance has not been created yet (provider_instance_id is empty).
var liveLaunchStatuses = map[string]bool{
	db.LaunchStatusLaunching: true, // provisioning on provider
	db.LaunchStatusRunning:   true, // billing
	db.LaunchStatusPaused:    true, // typically still billing for storage
	db.LaunchStatusGrace:     true, // grace period after failure; still billing
}

// LoadOpts controls what gets loaded into a Snapshot.
type LoadOpts struct {
	SpendTargetUSD  float64
	ProcessedFilter string // "", "processed", or "unprocessed" — matches db.ListJobsByStatuses
}

// LoadSnapshot reads the current state from the DB. It does no caching and is
// safe to call from a tea.Tick handler.
//
// We deliberately read from the DB rather than the existing `monitor.Monitor`
// because the dashboard is its own program; sharing the monitor would require
// starting it, which adds host sync side-effects we don't want for a read-only
// dashboard.
func LoadSnapshot(database *sql.DB, opts LoadOpts, logger *slog.Logger) Snapshot {
	if logger == nil {
		logger = logging.Discard()
	}
	now := time.Now()
	snap := Snapshot{
		LoadedAt:       now,
		SpendTargetUSD: opts.SpendTargetUSD,
	}

	// Active jobs: running + queued + pending_placement + starting. Plus
	// recent terminal jobs for context (Pulse, Tree, History). The
	// processed filter, when non-empty, hides processed (or unprocessed)
	// jobs to match the uj TUI's filter.
	activeStatuses := []string{
		db.StatusRunning, db.StatusStarting, db.StatusQueued, db.StatusPendingPlacement,
	}
	if active, err := db.ListJobsByStatuses(database, activeStatuses, "", "", 0, nil, opts.ProcessedFilter); err != nil {
		logger.Debug("dashtabs: list active jobs", "err", err)
	} else {
		snap.Jobs = append(snap.Jobs, active...)
	}
	// Recent terminal (last 6h) for at-a-glance failure/completion context.
	if recent, err := db.ListRecentTerminalJobs(database, now.Add(-6*time.Hour).Unix()); err != nil {
		logger.Debug("dashtabs: list recent terminal jobs", "err", err)
	} else {
		if opts.ProcessedFilter != "" {
			recent = db.FilterJobsByTags(recent, nil, opts.ProcessedFilter)
		}
		snap.Jobs = append(snap.Jobs, recent...)
	}
	snap.Counts = deriveCounts(snap.Jobs)

	if state, err := db.LoadAutopilotState(database); err != nil {
		logger.Debug("dashtabs: load autopilot state", "err", err)
	} else {
		snap.AutopilotState = state
	}

	if launches, err := db.ListNonTerminalLaunches(database); err != nil {
		logger.Debug("dashtabs: list non-terminal launches", "err", err)
	} else {
		for _, l := range launches {
			if l == nil {
				continue
			}
			if liveLaunchStatuses[l.Status] {
				snap.LiveInstances = append(snap.LiveInstances, l)
			} else {
				snap.StaleInstances = append(snap.StaleInstances, l)
			}
		}
		snap.SpendUSDPerHour = sumHourlySpend(snap.LiveInstances)
	}

	snap.RecentFailures = recentFailures(snap.CloudInstances(), snap.Jobs, now)

	// Recently-terminated cloud instances (24h window) — surfaced as the
	// "RECENT" section in the Fleet view when there's room.
	if rec, err := db.ListRecentlyTerminalLaunches(database, 24*time.Hour); err != nil {
		logger.Debug("dashtabs: list recently terminal launches", "err", err)
	} else {
		snap.RecentInstances = rec
	}

	// Live-state rows feed the ETA estimator's progress-based blend.
	if live, err := jobeta.LoadLaunchLiveStatesForJobs(database, snap.Jobs); err != nil {
		logger.Debug("dashtabs: load launch live states", "err", err)
	} else {
		snap.LaunchLiveByID = live
	}

	// Project spend over recent windows. ProjectSpend is sorted descending
	// by spend by the SQL query.
	if rows, err := db.ListProjectSpendSince(database, now.Add(-24*time.Hour).Unix()); err != nil {
		logger.Debug("dashtabs: project spend 24h", "err", err)
	} else {
		snap.ProjectSpend24h = rows
	}
	if rows, err := db.ListProjectSpendSince(database, now.Add(-7*24*time.Hour).Unix()); err != nil {
		logger.Debug("dashtabs: project spend 7d", "err", err)
	} else {
		snap.ProjectSpend7d = rows
	}

	// LLM API usage: rolled up by (feature, model) and daily for sparklines.
	if u, err := llmusage.ListFeatureUsageSince(database, now.Add(-24*time.Hour).Unix()); err != nil {
		logger.Debug("dashtabs: llm usage 24h", "err", err)
	} else {
		snap.LLMUsage24h = u
	}
	if u, err := llmusage.ListFeatureUsageSince(database, now.Add(-7*24*time.Hour).Unix()); err != nil {
		logger.Debug("dashtabs: llm usage 7d", "err", err)
	} else {
		snap.LLMUsage7d = u
	}
	if days, err := llmusage.DailySpendLastN(database, 30); err != nil {
		logger.Debug("dashtabs: llm daily spend", "err", err)
	} else {
		snap.LLMDailySpend = days
	}
	return snap
}

// LoadSnapshotWithHosts is like LoadSnapshot but also takes a host list (e.g.
// from a Monitor) since the DB doesn't store full host inventory. Pass nil to
// load without host data — the Fleet view will fall back to "(no host info)".
func LoadSnapshotWithHosts(database *sql.DB, hosts []*hostinfo.Host, opts LoadOpts, logger *slog.Logger) Snapshot {
	snap := LoadSnapshot(database, opts, logger)
	snap.Hosts = hosts
	return snap
}

func deriveCounts(jobs []*db.Job) StatusCounts {
	var c StatusCounts
	for _, j := range jobs {
		if j == nil {
			continue
		}
		switch j.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting:
			c.Running++
		case db.StatusQueued, db.StatusPendingPlacement:
			c.Queued++
		case db.StatusCompleted:
			c.Completed++
		case db.StatusFailed:
			c.Failed++
		case db.StatusKilled, db.StatusCanceled:
			c.Killed++
		default:
			c.Other++
		}
	}
	return c
}

// sumHourlySpend totals the per-hour cost across a slice of launches.
// Callers should pass only launches in a billing status (i.e. snap.LiveInstances).
func sumHourlySpend(launches []*db.Launch) float64 {
	var cents int
	for _, l := range launches {
		if l == nil {
			continue
		}
		cents += l.CostPerHourCents
	}
	return float64(cents) / 100.0
}

// recentFailures returns a summary of recently failed cloud instances grouped
// by provider, plus failed jobs in the last 24h.
func recentFailures(launches []*db.Launch, jobs []*db.Job, now time.Time) []FailureSummary {
	cutoff := now.Add(-24 * time.Hour).Unix()
	byProvider := map[string]*FailureSummary{}
	for _, l := range launches {
		if l == nil || l.EndedAt == nil || *l.EndedAt < cutoff {
			continue
		}
		switch l.TerminationReason {
		case "provider_failure", "infra_failure", "disk_full", "account_credit_exhausted":
			s, ok := byProvider[l.Provider]
			if !ok {
				s = &FailureSummary{Provider: l.Provider}
				byProvider[l.Provider] = s
			}
			s.Count++
			when := time.Unix(*l.EndedAt, 0)
			if when.After(s.When) {
				s.When = when
				s.Note = l.TerminationDetail
			}
		}
	}
	for _, j := range jobs {
		if j == nil || j.EffectiveStatus() != db.StatusFailed {
			continue
		}
		if j.EndTime == nil || *j.EndTime < cutoff {
			continue
		}
		key := "job:" + strings.TrimSpace(j.Host)
		s, ok := byProvider[key]
		if !ok {
			s = &FailureSummary{Provider: key}
			byProvider[key] = s
		}
		s.Count++
		when := time.Unix(*j.EndTime, 0)
		if when.After(s.When) {
			s.When = when
			s.Note = j.FailureReason
		}
	}
	out := make([]FailureSummary, 0, len(byProvider))
	for _, v := range byProvider {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// pushHistory advances the rolling window with a new sample at the head.
func pushHistory(h RecentHistory, now time.Time, snap Snapshot, sinceLastFailures int) RecentHistory {
	prepend := func(s []float64, v float64) []float64 {
		if len(s) >= HistoryLen {
			s = s[:HistoryLen-1]
		}
		return append([]float64{v}, s...)
	}
	prependI := func(s []int, v int) []int {
		if len(s) >= HistoryLen {
			s = s[:HistoryLen-1]
		}
		return append([]int{v}, s...)
	}
	prependT := func(s []time.Time, v time.Time) []time.Time {
		if len(s) >= HistoryLen {
			s = s[:HistoryLen-1]
		}
		return append([]time.Time{v}, s...)
	}
	h.Times = prependT(h.Times, now)
	h.QueueDepth = prependI(h.QueueDepth, snap.Counts.Queued)
	h.RunningCnt = prependI(h.RunningCnt, snap.Counts.Running)
	h.SpendPerHr = prepend(h.SpendPerHr, snap.SpendUSDPerHour)
	h.FailureCnt = prependI(h.FailureCnt, sinceLastFailures)
	return h
}
