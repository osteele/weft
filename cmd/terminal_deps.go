package cmd

import (
	"database/sql"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/ui/terminal"
)

func init() {
	terminal.SetDependencies(terminal.Dependencies{
		BuildCloudClients:                                 buildCloudClients,
		BuildR2Client:                                     buildR2Client,
		BuildPredictorConfig:                              buildPredictorConfig,
		BuildOverheadModel:                                buildOverheadModel,
		BuildSurvivalModel:                                buildSurvivalModel,
		CampaignActualCost:                                campaignActualCost,
		CollectJobsForList:                                collectJobsForList,
		CollectJobsForListWithFilters:                     collectJobsForListWithFilters,
		CreateOptsForProvider:                             createOptsForProvider,
		ExecuteReuseAssignments:                           executeReuseAssignments,
		FilterLaunchJobsByProject:                         filterLaunchJobsByProject,
		FilterRentalLaunchJobs:                            filterRentalLaunchJobs,
		KillOrCancelCloudJob:                              killOrCancelCloudJob,
		NewR2ClientFromConfig:                             newR2ClientFromConfig,
		PerformFastSync:                                   performFastSync,
		PerformSyncWithTimeoutForHostsDetailed:            performSyncWithTimeoutForHostsDetailed,
		PerformSyncWithTimeoutForHostsDetailedWithOptions: performSyncWithTimeoutForHostsDetailedWithOptions,
		AttemptRelaunchOrphanedJobs:                       orchestration.RelaunchOrphanedJobs,
		BuildStaleDataNote:                                buildStaleDataNote,
		PrintJobStatus:                                    func(job *db.Job, exitOnComplete bool) { printJobStatus(nil, job, exitOnComplete) },
		RefreshLaunchGroupsWithOnPrem:                     refreshLaunchGroupsWithOnPrem,
		SyncRentalJobsStatus:                              syncRentalJobsStatus,
		SyncCloudState:                                    terminalSyncCloudState,
		SyncCloudStateWithTimeout:                         terminalSyncCloudStateWithTimeout,
		SyncCloudStateWithTimeoutAndResults:               terminalSyncCloudStateWithTimeoutAndResults,
		SyncCloudStateWithClients:                         terminalSyncCloudStateWithClients,
		TerminateInstancesParallel:                        orchestration.TerminateInstancesParallel,
		PrintWatchExitReport:                              printWatchExitReport,
	})
}

func terminalSyncCloudState(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, verbose bool) terminal.CloudSyncResult {
	result := syncCloudState(cfg, database, reconciler, verbose)
	return terminal.CloudSyncResult(result)
}

func terminalSyncCloudStateWithTimeout(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, timeout time.Duration, verbose bool) (terminal.CloudSyncResult, bool) {
	result, completed := syncCloudStateWithTimeout(cfg, database, reconciler, timeout, verbose)
	return terminal.CloudSyncResult(result), completed
}

func terminalSyncCloudStateWithTimeoutAndResults(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, timeout time.Duration, verbose bool, syncResults bool) (terminal.CloudSyncResult, bool) {
	result, completed := syncCloudStateWithTimeoutAndResults(cfg, database, reconciler, timeout, verbose, syncResults)
	return terminal.CloudSyncResult(result), completed
}

func terminalSyncCloudStateWithClients(cfg *config.Config, database *sql.DB, reconciler *campaign.Reconciler, clients []cloud.Client, r2Client *r2.Client, verbose bool) terminal.CloudSyncResult {
	result := syncCloudStateWithClients(cfg, database, reconciler, clients, r2Client, verbose)
	return terminal.CloudSyncResult(result)
}
