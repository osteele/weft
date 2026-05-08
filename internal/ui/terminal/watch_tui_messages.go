package terminal

import (
	"github.com/osteele/weft/internal/app/dbwatch"
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
	instanceIDs         []int64
	skipped             int
	budgetSkip          int
	blockedReason       string
	notReplacedReasons  map[int64]string
	budgetBlockedByInst map[int64]bool
	err                 error
}

// retryBackoffMsg triggers a delayed retry attempt after no offers were found.
type retryBackoffMsg struct{}

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
	updateOnPremHosts  bool
	onPremHosts        []onPremHostSummary
	updateUnplacedJobs bool
	unplacedJobs       []*db.Job
	autoPassPhase      autoPilotPhaseHint
	err                error
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

type watchProcessDoneMsg struct {
	jobID   int64
	message string
	err     error
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
	source *dbwatch.Source
	err    error
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
	instanceIDs   []int64
	skipped       int
	budgetSkip    int
	blockedReason string
	reasons       map[int64]string
	err           error
}

// autoPilotBackoffReadyMsg fires when an auto-launch backoff delay elapses.
type autoPilotBackoffReadyMsg struct{}

// ---------------------------------------------------------------------------
// Move picker messages
// ---------------------------------------------------------------------------

// moveOptionsReadyMsg carries computed move destinations for the inline picker.
type moveOptionsReadyMsg struct {
	jobID   int64
	options []moveOption
	err     error
}

type moveExecuteAction string

const (
	moveExecuteActionMove      moveExecuteAction = "move"
	moveExecuteActionLaunchNew moveExecuteAction = "launch_new"
)

// moveExecuteDoneMsg carries the result of a move-to-existing or move-to-new execution.
type moveExecuteDoneMsg struct {
	jobID      int64
	targetDesc string // e.g. "instance #17" or "new RTX 4090 instance"
	action     moveExecuteAction
	err        error
}

// ---------------------------------------------------------------------------
// Render row types (system mode)
// ---------------------------------------------------------------------------

type watchRenderRow struct {
	text string
}
