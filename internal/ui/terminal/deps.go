package terminal

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/r2"
)

const (
	FastSyncTimeout        = 2 * time.Second
	DefaultSyncTimeout     = 5 * time.Second
	NormalSyncTimeout      = 30 * time.Second
	FastCloudSyncTimeout   = 10 * time.Second
	NormalCloudSyncTimeout = 60 * time.Second
	TerminalSyncInterval   = 60 * time.Second
)

type CloudSyncResult struct {
	Updated         int
	ReconcileResult *campaign.ReconcileResult
}

type cloudSyncResult = CloudSyncResult

type Dependencies struct {
	BuildCloudClients                                 func(*config.Config) ([]cloud.Client, error)
	BuildR2Client                                     func(*config.Config) (*r2.Client, error)
	BuildPredictorConfig                              func(*config.Config) predictor.Config
	BuildOverheadModel                                func(*sql.DB) *estimate.OverheadModel
	BuildSurvivalModel                                func(*sql.DB) *bidding.SurvivalModel
	CampaignActualCost                                func([]*db.Launch) string
	CollectJobsForList                                func(*sql.DB, []string) ([]*db.Job, error)
	CreateOptsForProvider                             func(*config.Config, cloud.Provider) (cloud.CreateOpts, error)
	ExecuteReuseAssignments                           func(*sql.DB, *r2.Client, []campaign.ReuseAssignment) error
	FilterLaunchJobsByProject                         func([]*db.Job) []*db.Job
	FilterRentalLaunchJobs                            func([]*db.Job) []*db.Job
	KillOrCancelCloudJob                              func(*sql.DB, int64, string) (string, error)
	NewR2ClientFromConfig                             func() (*r2.Client, error)
	PerformFastSync                                   func(*sql.DB, bool) (bool, []string, []string)
	PerformSyncWithTimeoutForHostsDetailed            func(*sql.DB, []string, time.Duration, bool) (bool, []string, []string, []string)
	PerformSyncWithTimeoutForHostsDetailedWithOptions func(*sql.DB, []string, time.Duration, bool, bool) (bool, []string, []string, []string)
	AttemptRelaunchOrphanedJobs                       func(*sql.DB, *config.Config, int, map[int64]float64, []int64, string, bool, bool) (*campaign.RelaunchResult, error)
	BuildStaleDataNote                                func(*sql.DB, []string, []string) string
	PrintJobStatus                                    func(*db.Job, bool)
	RefreshLaunchGroupsWithOnPrem                     func(*sql.DB, *config.Config, string, string, map[int64]bool, func(int, int), func(string)) ([]campaign.InstanceGroup, error)
	SyncRentalJobsStatus                              func(*sql.DB) bool
	SyncCloudState                                    func(*config.Config, *sql.DB, *campaign.Reconciler, bool) CloudSyncResult
	SyncCloudStateWithTimeout                         func(*config.Config, *sql.DB, *campaign.Reconciler, time.Duration, bool) (CloudSyncResult, bool)
	SyncCloudStateWithClients                         func(*config.Config, *sql.DB, *campaign.Reconciler, []cloud.Client, *r2.Client, bool) CloudSyncResult
	TerminateInstancesParallel                        func(*sql.DB, []int64) (int, []error)
	PrintWatchExitReport                              func(*sql.DB, []int64)
}

var deps Dependencies

func SetDependencies(d Dependencies) {
	deps = d
}

func buildCloudClients(cfg *config.Config) ([]cloud.Client, error) {
	if deps.BuildCloudClients == nil {
		return nil, nil
	}
	return deps.BuildCloudClients(cfg)
}

func buildR2Client(cfg *config.Config) (*r2.Client, error) {
	if deps.BuildR2Client == nil {
		return nil, nil
	}
	return deps.BuildR2Client(cfg)
}

func buildPredictorConfig(cfg *config.Config) predictor.Config {
	if deps.BuildPredictorConfig == nil {
		return predictor.Config{}
	}
	return deps.BuildPredictorConfig(cfg)
}

func buildOverheadModel(database *sql.DB) *estimate.OverheadModel {
	if deps.BuildOverheadModel == nil {
		return nil
	}
	return deps.BuildOverheadModel(database)
}

func buildSurvivalModel(database *sql.DB) *bidding.SurvivalModel {
	if deps.BuildSurvivalModel == nil {
		return nil
	}
	return deps.BuildSurvivalModel(database)
}

func campaignActualCost(instances []*db.Launch) string {
	if deps.CampaignActualCost == nil {
		return "$0.00"
	}
	return deps.CampaignActualCost(instances)
}

func collectJobsForList(database *sql.DB, args []string) ([]*db.Job, error) {
	if deps.CollectJobsForList == nil {
		return nil, fmt.Errorf("terminal dependencies are not configured")
	}
	return deps.CollectJobsForList(database, args)
}

func createOptsForProvider(cfg *config.Config, provider cloud.Provider) (cloud.CreateOpts, error) {
	if deps.CreateOptsForProvider == nil {
		return cloud.CreateOpts{}, fmt.Errorf("terminal dependencies are not configured")
	}
	return deps.CreateOptsForProvider(cfg, provider)
}

func executeReuseAssignments(database *sql.DB, r2Client *r2.Client, assignments []campaign.ReuseAssignment) error {
	if deps.ExecuteReuseAssignments == nil {
		return fmt.Errorf("terminal dependencies are not configured")
	}
	return deps.ExecuteReuseAssignments(database, r2Client, assignments)
}

func filterLaunchJobsByProject(jobs []*db.Job) []*db.Job {
	if deps.FilterLaunchJobsByProject == nil {
		return jobs
	}
	return deps.FilterLaunchJobsByProject(jobs)
}

func filterLaunchJobsForScope(jobs []*db.Job, projectFilter string) []*db.Job {
	if projectFilter != "" {
		return db.FilterJobsByProject(jobs, projectFilter)
	}
	return filterLaunchJobsByProject(jobs)
}

func filterRentalLaunchJobs(jobs []*db.Job) []*db.Job {
	if deps.FilterRentalLaunchJobs == nil {
		return jobs
	}
	return deps.FilterRentalLaunchJobs(jobs)
}

func newR2ClientFromConfig() (*r2.Client, error) {
	if deps.NewR2ClientFromConfig == nil {
		return nil, fmt.Errorf("terminal dependencies are not configured")
	}
	return deps.NewR2ClientFromConfig()
}

func killOrCancelCloudJob(database *sql.DB, jobID int64, targetStatus string) (string, error) {
	if deps.KillOrCancelCloudJob == nil {
		return "", nil
	}
	return deps.KillOrCancelCloudJob(database, jobID, targetStatus)
}

func performFastSync(database *sql.DB, verbose bool) (bool, []string, []string) {
	if deps.PerformFastSync == nil {
		return true, nil, nil
	}
	return deps.PerformFastSync(database, verbose)
}

func performSyncWithTimeoutForHostsDetailed(database *sql.DB, hosts []string, timeout time.Duration, verbose bool) (bool, []string, []string, []string) {
	if deps.PerformSyncWithTimeoutForHostsDetailed == nil {
		return true, nil, nil, nil
	}
	return deps.PerformSyncWithTimeoutForHostsDetailed(database, hosts, timeout, verbose)
}

func performSyncWithTimeoutForHostsDetailedWithOptions(database *sql.DB, hosts []string, timeout time.Duration, verbose bool, startQueueRunner bool) (bool, []string, []string, []string) {
	if deps.PerformSyncWithTimeoutForHostsDetailedWithOptions == nil {
		return true, nil, nil, nil
	}
	return deps.PerformSyncWithTimeoutForHostsDetailedWithOptions(database, hosts, timeout, verbose, startQueueRunner)
}

func attemptRelaunchOrphanedJobs(
	database *sql.DB,
	cfg *config.Config,
	extraAttempts int,
	retryBudgetMultiplierByFailedInstance map[int64]float64,
	scopeJobIDs []int64,
	scopeProject string,
	restrictToReset bool,
	includeFreshUnplaced bool,
) (*campaign.RelaunchResult, error) {
	if deps.AttemptRelaunchOrphanedJobs == nil {
		return &campaign.RelaunchResult{}, nil
	}
	return deps.AttemptRelaunchOrphanedJobs(
		database,
		cfg,
		extraAttempts,
		retryBudgetMultiplierByFailedInstance,
		scopeJobIDs,
		scopeProject,
		restrictToReset,
		includeFreshUnplaced,
	)
}

func buildStaleDataNote(database *sql.DB, unreachable, slow []string) string {
	if deps.BuildStaleDataNote == nil {
		return ""
	}
	return deps.BuildStaleDataNote(database, unreachable, slow)
}

func printJobStatus(job *db.Job, exitOnComplete bool) {
	if deps.PrintJobStatus != nil {
		deps.PrintJobStatus(job, exitOnComplete)
	}
}

func refreshLaunchGroupsWithOnPrem(database *sql.DB, cfg *config.Config, gpuFilter string, projectFilter string, jobIDFilter map[int64]bool, onProgress func(int, int), onPhase func(string)) ([]campaign.InstanceGroup, error) {
	if deps.RefreshLaunchGroupsWithOnPrem == nil {
		return nil, nil
	}
	return deps.RefreshLaunchGroupsWithOnPrem(database, cfg, gpuFilter, projectFilter, jobIDFilter, onProgress, onPhase)
}

func hasLaunchGroupsOnPremRefresh() bool {
	return deps.RefreshLaunchGroupsWithOnPrem != nil
}

func syncRentalJobsStatus(database *sql.DB) bool {
	if deps.SyncRentalJobsStatus == nil {
		return true
	}
	return deps.SyncRentalJobsStatus(database)
}

func syncCloudState(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, verbose bool) cloudSyncResult {
	if deps.SyncCloudState == nil {
		return cloudSyncResult{}
	}
	return deps.SyncCloudState(cfg, database, reconciler, verbose)
}

func syncCloudStateWithTimeout(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, timeout time.Duration, verbose bool) (cloudSyncResult, bool) {
	if deps.SyncCloudStateWithTimeout == nil {
		return cloudSyncResult{}, true
	}
	return deps.SyncCloudStateWithTimeout(cfg, database, reconciler, timeout, verbose)
}

func syncCloudStateWithClients(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, clients []cloud.Client, r2Client *r2.Client, verbose bool) cloudSyncResult {
	if deps.SyncCloudStateWithClients == nil {
		return cloudSyncResult{}
	}
	return deps.SyncCloudStateWithClients(cfg, database, reconciler, clients, r2Client, verbose)
}

func terminateInstancesParallel(database *sql.DB, ids []int64) (int, []error) {
	if deps.TerminateInstancesParallel == nil {
		return 0, nil
	}
	return deps.TerminateInstancesParallel(database, ids)
}

func printWatchExitReport(database *sql.DB, instanceIDs []int64) {
	if deps.PrintWatchExitReport != nil {
		deps.PrintWatchExitReport(database, instanceIDs)
	}
}

func cloudClientForProvider(clients []cloud.Client, provider cloud.Provider) cloud.Client {
	if provider == "" && len(clients) > 0 {
		return clients[0]
	}
	for _, client := range clients {
		if client.Provider() == provider {
			return client
		}
	}
	return nil
}
