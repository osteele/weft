package terminal

import (
	"database/sql"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/jobview"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/r2"
)

type Mode = watchMode

const (
	ModeCampaign  = watchModeCampaign
	ModeInstances = watchModeInstances
	ModeSystem    = watchModeSystem
	ModeProject   = watchModeProject
)

type ProjectGroup = projectGroup
type LaunchExecutionPlan = campaign.LaunchExecutionPlan
type CloudInstanceObservability = cloudInstanceObservability
type ObservedActivity = observedActivity
type ColumnDef = columnDef

var DefaultJSONColumnKeys = append([]string(nil), defaultJSONColumnKeys...)
var DefaultTSVColumnKeys = append([]string(nil), defaultTSVColumnKeys...)

type LaunchResult struct {
	Err             error
	InstanceIDs     []int64
	CostEstimates   []campaign.CostEstimate
	InlineWatchUsed bool
}

func RunWatchLoop(database *sql.DB, cfg *config.Config) error {
	router := newWatchRouterModel(database, cfg, "")

	stdio := InstallTUIStdioCapture()
	p := tea.NewProgram(router, stdio.Option, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus())
	finalModel, err := p.Run()
	stdio.Restore()

	var pendingExec func() error
	if r, ok := finalModel.(watchRouterModel); ok {
		if w, ok := r.active.(watchModel); ok && w.syncWorker != nil {
			w.syncWorker.Stop()
		}
		r.stopBanners()
		pendingExec = r.pendingExec
	}
	if err != nil {
		return fmt.Errorf("watch TUI error: %w", err)
	}
	// pendingExec is set by the schema-drift monitor when the on-disk
	// binary is newer than this process. Run it now that bubbletea has
	// restored the terminal.
	if pendingExec != nil {
		if execErr := pendingExec(); execErr != nil {
			return fmt.Errorf("schema-drift relaunch: %w", execErr)
		}
	}
	return nil
}

func RunLaunchProgram(database *sql.DB, cfg *config.Config, groups []campaign.InstanceGroup, opts campaign.LaunchOpts, gpuFilter string, jobIDFilter map[int64]bool, reconciling bool, fromWatch bool, inlineWatchEnabled bool) (LaunchResult, error) {
	clients, providerErr := buildCloudClients(cfg)
	if len(opts.AffinityMachines) > 0 && providerErr == nil {
		clients, providerErr = campaign.MachineIDClients(clients, "machine-constrained launches")
	} else if opts.DistinctMachines && providerErr == nil {
		clients, providerErr = campaign.DistinctMachineClients(clients)
	}
	if providerErr != nil {
		return LaunchResult{Err: providerErr}, nil
	}
	predCfg := buildPredictorConfig(cfg)
	model := newLaunchModel(database, clients, nil, cfg, groups, opts, &predCfg, gpuFilter, "", jobIDFilter, reconciling, fromWatch, inlineWatchEnabled)

	stdio := InstallTUIStdioCapture()
	p := tea.NewProgram(model, stdio.Option, tea.WithAltScreen(), tea.WithMouseCellMotion())
	finalModel, err := p.Run()
	stdio.Restore()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("launch TUI error: %w", err)
	}

	m, ok := finalModel.(launchModel)
	if !ok {
		return LaunchResult{}, nil
	}
	return LaunchResult{
		Err:             m.err,
		InstanceIDs:     append([]int64(nil), m.instanceIDs...),
		CostEstimates:   append([]campaign.CostEstimate(nil), m.costEstimates...),
		InlineWatchUsed: m.inlineWatchUsed,
	}, nil
}

func WatchInstances(database *sql.DB, mode Mode, instanceIDs []int64, estimateSummary *campaign.CostEstimateSummary, projectFilter string) ([]int64, error) {
	return watchInstances(database, mode, instanceIDs, estimateSummary, projectFilter)
}

func WatchInstancesPlain(database *sql.DB, mode Mode, instanceIDs []int64, estimateSummary *campaign.CostEstimateSummary, projectFilter string) error {
	return watchInstancesPlain(database, mode, instanceIDs, estimateSummary, projectFilter)
}

func WatchJobsPlain(database *sql.DB, jobIDs []int64, opts WatchPlainOptions) error {
	return watchJobsPlain(database, jobIDs, opts)
}

func WatchAllPlain(database *sql.DB, cfg *config.Config, opts WatchPlainOptions) error {
	return watchAllPlain(database, cfg, opts)
}

func RunCampaignListTUI(database *sql.DB, campaigns []*db.Campaign) error {
	return runCampaignListTUI(database, campaigns)
}

func RunProjectWatchTUI(database *sql.DB, cfg *config.Config, recentWindow time.Duration, syncEnabled bool, projectFilter string) error {
	router := newProjectWatchRouterModel(database, cfg, recentWindow, syncEnabled, projectFilter)

	stdio := InstallTUIStdioCapture()
	defer stdio.Restore()

	finalModel, err := tea.NewProgram(router, stdio.Option, tea.WithAltScreen(), tea.WithReportFocus()).Run()
	if r, ok := finalModel.(watchRouterModel); ok {
		if w, ok := r.active.(watchModel); ok && w.syncWorker != nil {
			w.syncWorker.Stop()
		}
	}
	if err != nil {
		return fmt.Errorf("run project watch TUI: %w", err)
	}
	return nil
}

func RunListTUI(database *sql.DB, args []string, jobs []*db.Job, title string, syncEnabled bool, groupedByStatus bool, projectFilter string) error {
	return runListTUI(database, args, jobs, title, syncEnabled, groupedByStatus, projectFilter)
}

// DashboardOptions controls initial state of the dashboard view (start tab,
// auto-rotate interval). All fields are optional.
type DashboardOptions struct {
	StartTabIndex int
	StartCycle    time.Duration
}

// RunDashboardTUI launches the tabbed dashboard as the initial panel of a
// watchRouter so the user can press 'L' to seamlessly swap to the list TUI
// (and 'D' from the list to come back) without re-entering alt-screen.
func RunDashboardTUI(database *sql.DB, cfg *config.Config, opts DashboardOptions) error {
	router := newDashboardWatchRouterModel(database, cfg, opts)

	stdio := InstallTUIStdioCapture()
	defer stdio.Restore()

	finalModel, err := tea.NewProgram(router, stdio.Option, tea.WithAltScreen(), tea.WithReportFocus()).Run()
	if r, ok := finalModel.(watchRouterModel); ok {
		r.cleanupActive()
		r.stopBanners()
	}
	if err != nil {
		return fmt.Errorf("run dashboard TUI: %w", err)
	}
	return nil
}

// RunHostsTUI launches the hosts TUI as the initial panel of a watchRouter,
// so the user can navigate to jobs/instances views via the usual switch keys.
func RunHostsTUI(database *sql.DB, cfg *config.Config) error {
	router := newHostsWatchRouterModel(database, cfg)

	stdio := InstallTUIStdioCapture()
	defer stdio.Restore()

	finalModel, err := tea.NewProgram(router, stdio.Option, tea.WithAltScreen(), tea.WithReportFocus()).Run()
	if r, ok := finalModel.(watchRouterModel); ok {
		if h, ok := r.active.(hostsTUIModel); ok {
			h.shutdown()
		}
	}
	if err != nil {
		return fmt.Errorf("run hosts TUI: %w", err)
	}
	return nil
}

func ListOutputWidth() int {
	return listOutputWidth()
}

func WriteListPlainOutput(output string) error {
	return writeListPlainOutput(output)
}

func RenderJobListPlain(jobs []*db.Job, width int) string {
	return renderJobListPlain(jobs, width)
}

func RenderJobListPlainWithOptions(jobs []*db.Job, width int, columnKeys []string, noTruncate bool) string {
	return renderJobListPlainWithOptions(jobs, width, columnKeys, noTruncate)
}

func RenderJobListGroupedStatusPlain(jobs []*db.Job, width int) string {
	return renderJobListGroupedStatusPlain(jobs, width)
}

func RenderJobListGroupedStatusPlainWithLiveState(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState) string {
	return renderJobListGroupedStatusPlainWithLiveState(jobs, width, launchLiveByID)
}

func RenderJobListGroupedStatusPlainWithLaunchState(jobs []*db.Job, width int, launchLiveByID map[int64]*db.LaunchLiveState, launchStatusByID map[int64]string) string {
	return renderJobListGroupedStatusPlainWithLaunchState(jobs, width, launchLiveByID, launchStatusByID)
}

func RenderJobListGroupedStatusPlainWithFailedInstances(
	database *sql.DB,
	jobs []*db.Job,
	width int,
	launchLiveByID map[int64]*db.LaunchLiveState,
	launchStatusByID map[int64]string,
) string {
	failures := loadRecentFailedInstances(database, recentFailedInstanceWindow, time.Now())
	launchByID, _ := db.GetLaunchesByIDs(database, groupedStatusLaunchIDs(jobs))
	attemptOutcomeByJob, _ := db.LatestAttemptOutcomeEvents(database, jobIDsForOutcomeEvents(jobs))
	return renderJobListGroupedStatusPlainWithOptions(jobs, width, groupedStatusRenderOptions{
		launchLiveByID:      launchLiveByID,
		launchStatusByID:    launchStatusByID,
		attemptOutcomeByJob: attemptOutcomeByJob,
		failedInstances:     failures,
		launchByID:          launchByID,
		now:                 time.Now(),
	})
}

func RenderJobListGroupedStatusPlainWithOptions(
	database *sql.DB,
	jobs []*db.Job,
	width int,
	launchLiveByID map[int64]*db.LaunchLiveState,
	launchStatusByID map[int64]string,
	placementStatusByJob map[int64]jobview.PlacementStatus,
) string {
	failures := loadRecentFailedInstances(database, recentFailedInstanceWindow, time.Now())
	launchByID, _ := db.GetLaunchesByIDs(database, groupedStatusLaunchIDs(jobs))
	attemptOutcomeByJob, _ := db.LatestAttemptOutcomeEvents(database, jobIDsForOutcomeEvents(jobs))
	return renderJobListGroupedStatusPlainWithOptions(jobs, width, groupedStatusRenderOptions{
		launchLiveByID:       launchLiveByID,
		launchStatusByID:     launchStatusByID,
		placementStatusByJob: placementStatusByJob,
		attemptOutcomeByJob:  attemptOutcomeByJob,
		failedInstances:      failures,
		launchByID:           launchByID,
		now:                  time.Now(),
	})
}

func ResolveColumns(keys []string, defaultKeys []string) ([]ColumnDef, error) {
	return resolveColumns(keys, defaultKeys)
}

func PrintJobsJSON(w io.Writer, jobs []*db.Job, cols []ColumnDef) error {
	return printJobsJSON(w, jobs, cols)
}

func PrintJobsTSV(w io.Writer, jobs []*db.Job, cols []ColumnDef) error {
	return printJobsTSV(w, jobs, cols)
}

func GroupJobsByProject(jobs []*db.Job) []ProjectGroup {
	return groupJobsByProject(jobs)
}

func RenderProjectListPlain(groups []ProjectGroup, width int) string {
	return renderProjectListPlain(groups, width)
}

func RenderProjectJobsPlain(groups []ProjectGroup, width int) string {
	return renderProjectJobsPlain(groups, width)
}

func RenderProjectWatchPlain(groups []ProjectGroup, width int, now time.Time, recentWindow time.Duration) string {
	return renderProjectWatchPlain(groups, width, now, recentWindow)
}

func PrepareLaunchExecutionPlan(database *sql.DB, clients []cloud.Client, providerErr error, groups []campaign.InstanceGroup, selected map[int64]bool, profile bidding.ScoreProfile, minSurvival float64, minReliability float64, predCfg *predictor.Config, overheadModel *estimate.OverheadModel, survivalModel *bidding.SurvivalModel) (LaunchExecutionPlan, error) {
	return campaign.PrepareLaunchExecutionPlan(database, clients, providerErr, groups, selected, profile, minSurvival, minReliability, predCfg, overheadModel, survivalModel)
}

func PrepareLaunchExecutionPlanWithProgress(database *sql.DB, clients []cloud.Client, providerErr error, groups []campaign.InstanceGroup, selected map[int64]bool, profile bidding.ScoreProfile, minSurvival float64, minReliability float64, predCfg *predictor.Config, overheadModel *estimate.OverheadModel, survivalModel *bidding.SurvivalModel, onProgress campaign.PlanProgressFunc) (LaunchExecutionPlan, error) {
	return campaign.PrepareLaunchExecutionPlanWithProgress(database, clients, providerErr, groups, selected, profile, minSurvival, minReliability, predCfg, overheadModel, survivalModel, onProgress)
}

func ObserveLaunch(ci *db.Launch, inst *cloud.Instance, now time.Time) CloudInstanceObservability {
	return observeLaunch(ci, inst, now)
}

func FormatObservedActivity(update campaign.InstanceUpdate, now time.Time) ObservedActivity {
	return formatObservedActivity(update, now)
}

func FormatUploadSummary(timings *db.JobPhaseTimings) string {
	return formatUploadSummary(timings)
}

func ReadCachedJobFailureExcerpt(jobID int64) string {
	return readCachedJobFailureExcerpt(jobID)
}

func CollectReplacementChain(ci *db.Launch, getCI func(int64) *db.Launch) []*db.Launch {
	return collectReplacementChain(ci, getCI)
}

func FormatPreviousInstanceLine(donors []*db.Launch, now time.Time) string {
	return formatPreviousInstanceLine(donors, now)
}

func FormatBytesIEC(n int64) string {
	return formatBytesIEC(n)
}

func BuildCampaignWatchModel(database *sql.DB, instanceIDs []int64, r2Client *r2.Client, cfg *config.Config) tea.Model {
	return newCampaignWatchModel(database, instanceIDs, r2Client, cfg)
}
