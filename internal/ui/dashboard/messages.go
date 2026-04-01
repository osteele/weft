package dashboard

import (
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/llm"
	"github.com/osteele/weft/internal/monitor"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/progress"
	"github.com/osteele/weft/internal/ssh"
)

// Messages
type jobsRefreshedMsg struct {
	jobs            []*db.Job
	jobDependencies map[int64]string // jobID -> dep_spec (e.g., "930" or "930+")
	err             error
}

type dbWatcherReadyMsg struct {
	watcher *fsnotify.Watcher
	targets map[string]struct{}
	err     error
}

type dbWatchEventMsg struct {
	err error
}

type dbRefreshTriggeredMsg struct{}

type monitorEventMsg struct {
	event monitor.Event
}

type syncCompletedMsg struct {
	updated           int
	queuesStarted     []string // hosts where queue runners were started
	hostsSynced       []string
	queueRunnerErrors []string
	err               error
}

type logFetchedMsg struct {
	jobID     int64
	content   string
	progress  *progress.Progress // extracted progress info (nil if none found)
	err       error
	connError bool // true if this was a connection error (host unreachable)
	fromCache bool // true if this log was served from local cache
}

type quickProgressMsg struct {
	jobID    int64
	progress *progress.Progress
}

type jobKilledMsg struct {
	jobID     int64
	err       error
	deferred  bool // true if kill was queued for later (host offline)
	cancelled bool // true if this was a queued job that was cancelled
	message   string
}

type jobPausedMsg struct {
	jobID    int64
	err      error
	deferred bool
	message  string
}

type jobResumedMsg struct {
	jobID    int64
	err      error
	deferred bool
	message  string
}

type jobDraftedMsg struct {
	jobID    int64
	err      error
	deferred bool
	message  string
}

type jobQueuedMsg struct {
	jobID    int64
	err      error
	deferred bool
	message  string
}

type jobRestartedMsg struct {
	jobID    int64
	err      error
	deferred bool // true if restart was queued for later (host offline)
}

type jobRetriedMsg struct {
	jobID    int64
	err      error
	deferred bool
}

type jobStartedNowMsg struct {
	jobID    int64
	host     string
	deferred bool
	err      error
}

type pruneCompletedMsg struct {
	count int64
	err   error
}

type queueStartedMsg struct {
	host    string
	already bool // true if queue was already running
	err     error
}

type jobMovedToFrontMsg struct {
	jobID    int64
	host     string
	moved    bool // false if already at front
	deferred bool
	err      error
}

type jobRemovedMsg struct {
	jobID int64
	err   error
}

type jobCreatedMsg struct {
	jobID    int64
	err      error
	deferred bool // true if job creation was queued for later (host offline)
}

type jobEditedMsg struct {
	jobID    int64
	host     string
	deferred bool
	err      error
}

type jobCreateProgressMsg struct {
	step string
}

type tickMsg time.Time
type logTickMsg time.Time
type createTickMsg time.Time
type hostRefreshTickMsg time.Time
type hostSummaryTickMsg time.Time

// Host-related messages
type hostsLoadedMsg struct {
	hostNames []string
	err       error
}

type hostInfoMsg struct {
	hostName string
	info     *Host
}

type hostDeletedMsg struct {
	hostName string
	err      error
}

type cpuTopMsg struct {
	host      string
	processes []ssh.TopProcess
	err       error
	jobView   bool
}

type descriptionGeneratedMsg struct {
	jobID       int64
	description string
	err         error
}

type hostSummaryGeneratedMsg struct {
	host    string
	summary string
	hash    string
	err     error
}

type jobEnvLoadedMsg struct {
	jobID   int64
	envVars []string
	depSpec string
	err     error
}

type hostSyncTimesLoadedMsg struct {
	times map[string]time.Time
	err   error
}

type cloudDiscoveryLoadedMsg struct {
	clients []cloud.Client
	err     error
}

type llmGeneratorLoadedMsg struct {
	generator *llm.DescriptionGenerator
}

// Cloud menu messages
type cloudOffersLoadedMsg struct {
	job       *db.Job
	offerings []placement.CloudOffering
	err       error
}

type cloudJobLaunchedMsg struct {
	jobID      int64
	instanceID int
	err        error
}

type cloudJobProgressMsg struct {
	jobID int64
	phase string // "creating", "syncing", "running", "collecting", "destroying"
}

type cloudJobCompletedMsg struct {
	jobID    int64
	exitCode int
	cost     float64
	err      error
}
