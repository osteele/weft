package narrate

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/status"
)

// Snapshot is a point-in-time view of the bits of weft state we narrate.
type Snapshot struct {
	Time      time.Time
	Jobs      map[int64]JobView
	Instances map[int64]InstanceView
	Autopilot AutopilotView
}

// JobView holds the fields a narrator cares about. We deliberately drop
// columns that don't affect transitions to keep prompts small.
type JobView struct {
	ID                 int64
	Status             string
	Host               string
	Project            string
	Command            string
	ExitCode           *int
	StartTime          int64
	EndTime            *int64
	LaunchID           *int64
	Tags               []string
	PlacementReasons   []string // why this job is currently unplaced (if any)
	QueueBlockedReason string   // transient queue-gate reason
	FailureReason      string   // normalized failure reason (e.g. "timeout", "oom")
	ErrorMessage       string
}

// InstanceView is the trimmed Launch.
type InstanceView struct {
	ID                int64
	CampaignID        *int64
	Status            string
	Provider          string
	GPUSpec           string
	NumGPUs           int
	CostPerHourCents  int
	CreatedAt         int64
	LaunchedAt        *int64
	EndedAt           *int64
	GraceDeadline     *int64
	TerminationReason string
	TerminationDetail string
	Cordoned          bool
}

type AutopilotView struct {
	State          string // "never", "idle", "running", "stale", "paused"
	Paused         bool
	PausedReason   string
	PassAgeSeconds int64
	LastSummary    string
	LastError      string
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

	jobs, err := db.ListJobsByStatuses(database, activeJobStatuses, "", opts.Project, 0, nil, "")
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	for _, j := range jobs {
		snap.Jobs[j.ID] = jobToView(j)
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

func jobToView(j *db.Job) JobView {
	return JobView{
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
		PlacementReasons:   append([]string(nil), j.PlacementReasons...),
		QueueBlockedReason: j.QueueBlockedReason,
		FailureReason:      j.FailureReason,
		ErrorMessage:       j.ErrorMessage,
	}
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
	JobAdded     []JobView
	JobChanged   []JobChange
	JobRemoved   []int64   // raw IDs of jobs no longer in the active set
	JobFinished  []JobView // resolved terminal jobs (status, project, exit code populated)
	InstAdded    []InstanceView
	InstChanged  []InstanceChange
	InstRemoved  []int64
	AutopilotOld AutopilotView
	AutopilotNew AutopilotView
}

type JobChange struct {
	Before JobView
	After  JobView
}

type InstanceChange struct {
	Before InstanceView
	After  InstanceView
}

// Empty reports whether the delta has nothing worth narrating.
func (d Delta) Empty() bool {
	return len(d.JobAdded) == 0 && len(d.JobChanged) == 0 && len(d.JobRemoved) == 0 && len(d.JobFinished) == 0 &&
		len(d.InstAdded) == 0 && len(d.InstChanged) == 0 && len(d.InstRemoved) == 0 &&
		d.AutopilotOld.State == d.AutopilotNew.State
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
			d.JobFinished = append(d.JobFinished, jobToView(j))
		}
	}
	d.JobRemoved = nil
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
