package terminal

import (
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

// ---------------------------------------------------------------------------
// Message types
// ---------------------------------------------------------------------------

type watchUpdateMsg struct {
	instanceID int64
	update     campaign.InstanceUpdate
	closed     bool
}

// watchSyncTickMsg triggers periodic cloud job result syncing (instance-based modes).
type watchSyncTickMsg struct{}

// watchSyncDoneMsg is sent after syncCloudJobResults completes (instance-based modes).
type watchSyncDoneMsg struct{}

// watchJobsRefreshedMsg carries refreshed job lists from a background DB query.
type watchJobsRefreshedMsg struct {
	cloudInstances map[int64]*db.Launch
	jobs           map[int64][]*db.Job
	outcomes       map[int64]map[int64]string
	quitAfter      bool
}

// watchCheckDoneMsg triggers a periodic DB-based check for all-terminal state.
type watchCheckDoneMsg struct{}

// watchCheckDoneResultMsg carries the result of a background DB terminal check.
type watchCheckDoneResultMsg struct{ allTerminal bool }

// watchInstanceSyncResultMsg is sent when the background SyncWorker produces a result (instance-based modes).
type watchInstanceSyncResultMsg struct{}

// retryResultMsg carries the result of retrying failed instances.
type retryResultMsg struct {
	instanceIDs []int64
	skipped     int
	err         error
}

// retryBackoffMsg triggers a delayed retry attempt after no offers were found.
type retryBackoffMsg struct{}

// retryBackoffDelays defines the delay before each retry attempt.
var retryBackoffDelays = []time.Duration{
	30 * time.Second,
	60 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
}

// System-mode messages
type watchAllTickMsg struct{}

type watchAllRefreshedMsg struct {
	snapshot watchSystemSnapshot
	err      error
}

type watchSyncResultMsg struct {
	result hostsync.Result
}

type watchOnPremRefreshedMsg struct {
	onPremHosts  []onPremHostSummary
	unplacedJobs []*db.Job
	err          error
}

type watchUnplaceDoneMsg struct {
	job     *db.Job
	message string
	err     error
}

type watchSubmitDoneMsg struct {
	jobID      int64
	instanceID int64
	err        error
}

type watchKillDoneMsg struct {
	jobID   int64
	message string
	err     error
}

type watchTerminateDoneMsg struct {
	instanceID int64
	message    string
	err        error
}

// ---------------------------------------------------------------------------
// Project-mode messages
// ---------------------------------------------------------------------------

type watchProjectLoadedMsg struct {
	groups []projectGroup
	err    error
}

type watchProjectSyncFinishedMsg struct {
	warnings []string
	full     bool
}

type watchProjectSyncResultMsg struct {
	result hostsync.Result
}

type watchDBWatcherReadyMsg struct {
	watcher *fsnotify.Watcher
	targets map[string]struct{}
	err     error
}

type watchDBWatchEventMsg struct {
	err error
}

type watchProjectDBRefreshTriggeredMsg struct{}

type watchInstanceDBRefreshTriggeredMsg struct{}

type watchProjectSyncTickMsg struct{}

// ---------------------------------------------------------------------------
// Auto-pilot messages
// ---------------------------------------------------------------------------

// autoPlaceDoneMsg carries the result of auto-placing a single job.
type autoPlaceDoneMsg struct {
	jobID      int64
	instanceID int64
	err        error
}

// autoLaunchDoneMsg carries the result of an auto-launch attempt.
type autoLaunchDoneMsg struct {
	instanceIDs []int64
	skipped     int
	err         error
}

// ---------------------------------------------------------------------------
// Move picker messages
// ---------------------------------------------------------------------------

// moveOptionsReadyMsg carries computed move destinations for the inline picker.
type moveOptionsReadyMsg struct {
	jobID   int64
	options []moveOption
	err     error
}

// moveExecuteDoneMsg carries the result of a move-to-existing or move-to-new execution.
type moveExecuteDoneMsg struct {
	jobID      int64
	targetDesc string // e.g. "instance #17" or "new RTX 4090 instance"
	err        error
}

// ---------------------------------------------------------------------------
// Render row types (system mode)
// ---------------------------------------------------------------------------

type watchRenderRow struct {
	text string
}
