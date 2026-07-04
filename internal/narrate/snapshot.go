package narrate

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/explain"
	"github.com/osteele/weft/internal/jobview"
	"github.com/osteele/weft/internal/status"
)

// Snapshot is a point-in-time view of the bits of weft state we narrate.
type Snapshot struct {
	Time      time.Time              `json:"time"`
	Jobs      map[int64]JobView      `json:"jobs"`
	Instances map[int64]InstanceView `json:"instances"`
	Autopilot AutopilotView          `json:"autopilot"`
}

// JobView holds the fields a narrator cares about. We deliberately drop
// columns that don't affect transitions to keep prompts small.
type JobView struct {
	ID                 int64    `json:"id"`
	Status             string   `json:"status"`
	Host               string   `json:"host,omitempty"`
	Project            string   `json:"project,omitempty"`
	Command            string   `json:"command,omitempty"`
	ExitCode           *int     `json:"exit_code,omitempty"`
	StartTime          int64    `json:"start_time,omitempty"`
	EndTime            *int64   `json:"end_time,omitempty"`
	LaunchID           *int64   `json:"instance_id,omitempty"`
	Tags               []string `json:"tags,omitempty"`
	PlacementBucket    string   `json:"placement_bucket,omitempty"`
	PlacementAt        int64    `json:"placement_at,omitempty"`
	PlacementReasons   []string `json:"placement_reasons,omitempty"`    // why this job is currently unplaced (if any)
	QueueBlockedReason string   `json:"queue_blocked_reason,omitempty"` // transient queue-gate reason
	Explanation        string   `json:"explanation,omitempty"`          // normalized current-state explanation
	SuggestedAction    string   `json:"suggested_action,omitempty"`     // wait/replan/retry/etc. when known
	FailureReason      string   `json:"failure_reason,omitempty"`       // normalized failure reason (e.g. "timeout", "oom")
	ErrorMessage       string   `json:"error_message,omitempty"`
	ProgressPct        int      `json:"progress_pct,omitempty"`   // 0-100, -1 if unavailable
	ProgressPhase      int      `json:"progress_phase,omitempty"` // 1-based phase number, 0 if unknown/single-phase
}

// InstanceView is the trimmed Launch.
type InstanceView struct {
	ID                int64  `json:"id"`
	CampaignID        *int64 `json:"campaign_id,omitempty"`
	Status            string `json:"status"`
	Provider          string `json:"provider,omitempty"`
	GPUSpec           string `json:"gpu_spec,omitempty"`
	NumGPUs           int    `json:"num_gpus,omitempty"`
	CostPerHourCents  int    `json:"cost_per_hour_cents,omitempty"`
	CreatedAt         int64  `json:"created_at,omitempty"`
	LaunchedAt        *int64 `json:"launched_at,omitempty"`
	EndedAt           *int64 `json:"ended_at,omitempty"`
	GraceDeadline     *int64 `json:"grace_deadline,omitempty"`
	TerminationReason string `json:"termination_reason,omitempty"`
	TerminationDetail string `json:"termination_detail,omitempty"`
	Cordoned          bool   `json:"cordoned,omitempty"`
}

type AutopilotView struct {
	State          string `json:"state"` // "never", "idle", "running", "stale", "paused"
	Paused         bool   `json:"paused,omitempty"`
	PausedReason   string `json:"paused_reason,omitempty"`
	PassAgeSeconds int64  `json:"pass_age_seconds,omitempty"`
	LastSummary    string `json:"last_summary,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// SnapshotOptions narrows the snapshot to a project.
type SnapshotOptions struct {
	Project string
}

// activeJobStatuses enumerates statuses we always include in the snapshot.
// Terminal jobs are dropped from the live snapshot but are surfaced via the
// delta if they transitioned within this tick.
var activeJobStatuses = []string{
	status.Queued, status.PendingPlacement, status.Starting, status.Running, status.Paused,
}

// BuildSnapshot reads the DB and assembles a Snapshot. It includes:
//   - active jobs (queued / running / starting / paused / pending_placement)
//   - all non-completed instances
//   - all active campaigns
//   - autopilot state
func BuildSnapshot(database *sql.DB, opts SnapshotOptions) (*Snapshot, error) {
	now := time.Now()
	snap := &Snapshot{
		Time:      now,
		Jobs:      make(map[int64]JobView),
		Instances: make(map[int64]InstanceView),
	}

	jobs, err := db.ListJobsByStatuses(database, activeJobStatuses, "", opts.Project, 0, nil, "unprocessed")
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	launchIDs := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		if j != nil && j.LaunchID != nil && *j.LaunchID > 0 {
			launchIDs = append(launchIDs, *j.LaunchID)
		}
	}
	liveByLaunch, err := db.GetLaunchLiveStates(database, launchIDs)
	if err != nil {
		return nil, fmt.Errorf("load launch live states: %w", err)
	}
	placementStatusByJob, err := jobview.PlacementStatusForJobs(database, jobs, now)
	if err != nil {
		return nil, fmt.Errorf("load placement status: %w", err)
	}
	for _, j := range jobs {
		snap.Jobs[j.ID] = jobToView(database, j, liveByLaunch, placementStatusByJob[j.ID])
	}

	instances, err := db.ListNonTerminalLaunches(database)
	if err != nil {
		return nil, fmt.Errorf("list launches: %w", err)
	}
	for _, inst := range instances {
		snap.Instances[inst.ID] = instanceToView(inst)
	}

	apState, err := db.LoadAutopilotState(database)
	if err != nil {
		return nil, fmt.Errorf("load autopilot state: %w", err)
	}
	snap.Autopilot = autopilotToView(apState, now)

	return snap, nil
}

func jobToView(database *sql.DB, j *db.Job, liveByLaunch map[int64]*db.LaunchLiveState, placementStatus jobview.PlacementStatus) JobView {
	view := JobView{
		ID:                 j.ID,
		Status:             j.Status,
		Host:               j.Host,
		Project:            j.Project,
		Command:            truncateCommand(j.Command),
		ExitCode:           j.ExitCode,
		StartTime:          j.StartTime,
		EndTime:            j.EndTime,
		LaunchID:           j.LaunchID,
		Tags:               append([]string(nil), j.Tags...),
		PlacementBucket:    string(placementStatus.Bucket),
		PlacementAt:        placementStatus.DisplayAt,
		PlacementReasons:   append([]string(nil), j.PlacementReasons...),
		QueueBlockedReason: j.QueueBlockedReason,
		FailureReason:      j.FailureReason,
		ErrorMessage:       j.ErrorMessage,
		ProgressPct:        -1,
	}
	x := explain.ForJob(database, j, time.Now())
	view.Explanation = explain.SummaryLine(x)
	view.SuggestedAction = x.SuggestedAction
	if j.LaunchID != nil && liveByLaunch != nil {
		if live := liveByLaunch[*j.LaunchID]; live != nil && live.JobProgressID == j.ID && live.JobProgressPct >= 0 {
			view.ProgressPct = live.JobProgressPct
			view.ProgressPhase = live.JobProgressPhase
		}
	}
	return view
}

func instanceToView(c *db.Launch) InstanceView {
	return InstanceView{
		ID:                c.ID,
		CampaignID:        c.CampaignID,
		Status:            c.Status,
		Provider:          c.Provider,
		GPUSpec:           c.GPUSpec,
		NumGPUs:           c.NumGPUs,
		CostPerHourCents:  c.CostPerHourCents,
		CreatedAt:         c.CreatedAt,
		LaunchedAt:        c.LaunchedAt,
		EndedAt:           c.EndedAt,
		GraceDeadline:     c.GraceDeadline,
		TerminationReason: c.TerminationReason,
		TerminationDetail: c.TerminationDetail,
		Cordoned:          c.Cordoned,
	}
}

func autopilotToView(s *db.AutopilotState, now time.Time) AutopilotView {
	v := AutopilotView{State: "never"}
	if s == nil {
		return v
	}
	v.Paused = s.Paused
	v.PausedReason = s.PausedReason
	v.LastSummary = s.LastPassSummary
	v.LastError = s.LastPassError
	const staleAfter = 5 * time.Minute
	switch {
	case s.Paused:
		v.State = "paused"
	case s.IsActive(now, staleAfter):
		v.State = "running"
	case s.IsStale(now, staleAfter):
		v.State = "stale"
	case s.LastPassFinishedAt.IsZero() && s.PassStartedAt.IsZero():
		v.State = "never"
	default:
		v.State = "idle"
	}
	if !s.PassStartedAt.IsZero() {
		v.PassAgeSeconds = int64(now.Sub(s.PassStartedAt) / time.Second)
	}
	return v
}

func truncateCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	const max = 200
	if len(cmd) <= max {
		return cmd
	}
	return cmd[:max] + "…"
}

// Delta describes transitions between two snapshots.
type Delta struct {
	JobAdded       []JobView        `json:"job_added,omitempty"`
	JobChanged     []JobChange      `json:"job_changed,omitempty"`
	JobRemoved     []int64          `json:"job_removed,omitempty"`  // raw IDs of jobs no longer in the active set
	JobFinished    []JobView        `json:"job_finished,omitempty"` // resolved terminal jobs (status, project, exit code populated)
	InstAdded      []InstanceView   `json:"instance_added,omitempty"`
	InstChanged    []InstanceChange `json:"instance_changed,omitempty"`
	InstRemoved    []int64          `json:"instance_removed,omitempty"`    // raw IDs of instances no longer in the active set
	InstTerminated []InstanceView   `json:"instance_terminated,omitempty"` // resolved terminal instances (TerminationReason/Detail populated)
	AutopilotOld   AutopilotView    `json:"autopilot_old"`
	AutopilotNew   AutopilotView    `json:"autopilot_new"`
}

type JobChange struct {
	Before JobView `json:"before"`
	After  JobView `json:"after"`
}

type InstanceChange struct {
	Before InstanceView `json:"before"`
	After  InstanceView `json:"after"`
}

// Empty reports whether the delta has nothing worth narrating.
func (d Delta) Empty() bool {
	return len(d.JobAdded) == 0 && len(d.JobChanged) == 0 && len(d.JobRemoved) == 0 && len(d.JobFinished) == 0 &&
		len(d.InstAdded) == 0 && len(d.InstChanged) == 0 && len(d.InstRemoved) == 0 && len(d.InstTerminated) == 0 &&
		!autopilotChangeNeedsNarration(d.AutopilotOld, d.AutopilotNew)
}

func autopilotChangeNeedsNarration(old, next AutopilotView) bool {
	if old.State == next.State {
		return false
	}
	return old.State == "paused" || next.State == "paused"
}

// ResolveRemovedInstances replaces the raw InstRemoved id list with
// resolved InstanceView entries (InstTerminated) by querying the DB. This
// surfaces termination_reason and termination_detail to the model so the
// causal link between an instance failure and a job requeue is explicit
// rather than something the model has to guess from sibling jobs'
// placement_blocked_reasons.
func (d *Delta) ResolveRemovedInstances(database *sql.DB) error {
	if len(d.InstRemoved) == 0 {
		return nil
	}
	launches, err := db.GetLaunchesByIDs(database, d.InstRemoved)
	if err != nil {
		return err
	}
	for _, id := range d.InstRemoved {
		if l, ok := launches[id]; ok {
			d.InstTerminated = append(d.InstTerminated, instanceToView(l))
		}
	}
	d.InstRemoved = nil
	return nil
}

// ResolveRemovedJobs replaces the raw JobRemoved id list with resolved
// JobView entries (JobFinished) by querying the DB for each removed job's
// current row. Use this so the model sees terminal status transitions
// (e.g. "completed" / "failed") rather than an opaque "removed from active".
func (d *Delta) ResolveRemovedJobs(database *sql.DB) error {
	if len(d.JobRemoved) == 0 {
		return nil
	}
	jobs, err := db.GetJobsByIDs(database, d.JobRemoved)
	if err != nil {
		return err
	}
	for _, id := range d.JobRemoved {
		if j, ok := jobs[id]; ok {
			d.JobFinished = append(d.JobFinished, jobToView(database, j, nil, jobview.PlacementStatus{}))
		}
	}
	d.JobRemoved = nil
	return nil
}

// AddRecentTerminalJobs adds terminal jobs that appeared since prev.Time but
// were not present in either snapshot. This catches short-lived jobs that start
// and finish between activity ticks.
func (d *Delta) AddRecentTerminalJobs(database *sql.DB, prev *Snapshot, project string) error {
	if d == nil || prev == nil || prev.Time.IsZero() {
		return nil
	}
	jobs, err := db.ListRecentTerminalJobs(database, prev.Time.Unix())
	if err != nil {
		return err
	}
	if project != "" {
		jobs = db.FilterJobsByProject(jobs, project)
	}
	jobs = db.FilterJobsByTags(jobs, nil, "unprocessed")
	seen := make(map[int64]struct{}, len(d.JobFinished)+len(d.JobChanged))
	for _, job := range d.JobFinished {
		seen[job.ID] = struct{}{}
	}
	for _, change := range d.JobChanged {
		seen[change.After.ID] = struct{}{}
	}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if _, ok := prev.Jobs[job.ID]; ok {
			continue
		}
		if _, ok := seen[job.ID]; ok {
			continue
		}
		d.JobFinished = append(d.JobFinished, jobToView(database, job, nil, jobview.PlacementStatus{}))
		seen[job.ID] = struct{}{}
	}
	sortDelta(d)
	return nil
}

// DiffSnapshots computes the transitions from prev to next. A nil prev is
// treated as an empty snapshot (so the first tick is a single bulk-add).
func DiffSnapshots(prev, next *Snapshot) Delta {
	var d Delta
	if prev == nil {
		prev = &Snapshot{
			Jobs:      map[int64]JobView{},
			Instances: map[int64]InstanceView{},
		}
	}
	d.AutopilotOld = prev.Autopilot
	d.AutopilotNew = next.Autopilot

	// Jobs
	for id, after := range next.Jobs {
		before, ok := prev.Jobs[id]
		if !ok {
			d.JobAdded = append(d.JobAdded, after)
			continue
		}
		if jobChanged(before, after) {
			d.JobChanged = append(d.JobChanged, JobChange{Before: before, After: after})
		}
	}
	for id := range prev.Jobs {
		if _, ok := next.Jobs[id]; !ok {
			d.JobRemoved = append(d.JobRemoved, id)
		}
	}

	// Instances
	for id, after := range next.Instances {
		before, ok := prev.Instances[id]
		if !ok {
			d.InstAdded = append(d.InstAdded, after)
			continue
		}
		if instChanged(before, after) {
			d.InstChanged = append(d.InstChanged, InstanceChange{Before: before, After: after})
		}
	}
	for id := range prev.Instances {
		if _, ok := next.Instances[id]; !ok {
			d.InstRemoved = append(d.InstRemoved, id)
		}
	}

	sortDelta(&d)
	return d
}

func jobChanged(a, b JobView) bool {
	if a.Status != b.Status {
		return true
	}
	if a.Host != b.Host {
		return true
	}
	if !equalIntPtr(a.ExitCode, b.ExitCode) {
		return true
	}
	if !equalInt64Ptr(a.LaunchID, b.LaunchID) {
		return true
	}
	if a.PlacementBucket != b.PlacementBucket {
		return true
	}
	if a.PlacementAt != b.PlacementAt {
		return true
	}
	return false
}

func instChanged(a, b InstanceView) bool {
	if a.Status != b.Status {
		return true
	}
	if a.TerminationReason != b.TerminationReason {
		return true
	}
	if a.Cordoned != b.Cordoned {
		return true
	}
	if !equalInt64Ptr(a.GraceDeadline, b.GraceDeadline) {
		return true
	}
	return false
}

func equalIntPtr(a, b *int) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func equalInt64Ptr(a, b *int64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func sortDelta(d *Delta) {
	sort.Slice(d.JobAdded, func(i, j int) bool { return d.JobAdded[i].ID < d.JobAdded[j].ID })
	sort.Slice(d.JobChanged, func(i, j int) bool { return d.JobChanged[i].After.ID < d.JobChanged[j].After.ID })
	sort.Slice(d.JobRemoved, func(i, j int) bool { return d.JobRemoved[i] < d.JobRemoved[j] })
	sort.Slice(d.InstAdded, func(i, j int) bool { return d.InstAdded[i].ID < d.InstAdded[j].ID })
	sort.Slice(d.InstChanged, func(i, j int) bool { return d.InstChanged[i].After.ID < d.InstChanged[j].After.ID })
	sort.Slice(d.InstRemoved, func(i, j int) bool { return d.InstRemoved[i] < d.InstRemoved[j] })
	sort.Slice(d.JobFinished, func(i, j int) bool { return d.JobFinished[i].ID < d.JobFinished[j].ID })
}
