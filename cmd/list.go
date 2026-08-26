package cmd

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/jobview"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/syncorch"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/osteele/weft/internal/util"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:     "list [jobs|campaigns|instances|hosts|queues|artifacts|projects] [flags]",
	Aliases: []string{"ls"},
	Short:   "List jobs, campaigns, instances, hosts, queues, artifacts, or projects",
	Long: `List resources. Without a subcommand, lists jobs (same as "weft list jobs").

Subcommands:
  jobs        List and search job history (default)
  campaigns   List campaigns with instance counts
  instances   List cloud instances
  hosts       List known hosts
  queues      List job queues
  artifacts   List job output artifacts
  projects    List projects with summary stats

Examples:
  weft list                    # Recent jobs (last 7 days)
  weft list jobs --running     # Running jobs only
  weft list campaigns          # All campaigns
  weft list instances          # All cloud instances
  weft list hosts              # Known hosts
  weft list projects           # Projects with job counts
  weft list --all              # All jobs including older
  weft list --queued           # Jobs waiting in queue
  weft list jobs --group-by status --unprocessed  # Grouped status sections
  weft list jobs --group-by project               # Grouped project sections`,
	RunE: runList,
}

var (
	listRunning     bool
	listCompleted   bool
	listQueued      bool
	listDead        bool
	listFailed      bool
	listStatus      string
	listHost        string
	listSince       string
	listMine        bool
	listActive      bool
	listAllHosts    bool
	listSearch      string
	listLimit       int
	listShow        int64
	listShowRaw     string
	listCleanup     int
	listSync        bool
	listNoSync      bool
	listAll         bool
	listTags        []string
	listExcludeTags []string
	listProject     string
	listProcessed   bool
	listUnprocessed bool
	listRental      bool
	listInventory   bool
	listCloud       bool
	listWatch       bool
	listTUI         bool
	listPlain       bool
	listFormat      string
	listNoTruncate  bool
	listColumns     []string
	listGroupBy     string
)

const defaultHostSyncWindow = 48 * time.Hour

// addListQueryFlags registers list query/filter flags shared by job list and
// project jobs.
func addListQueryFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&listRunning, "running", false, "Show only running jobs")
	cmd.Flags().BoolVar(&listCompleted, "completed", false, "Show only completed jobs")
	cmd.Flags().BoolVar(&listQueued, "queued", false, "Show only queued jobs (waiting in queue)")
	cmd.Flags().BoolVar(&listDead, "dead", false, "Show only dead jobs")
	cmd.Flags().BoolVar(&listFailed, "failed", false, "Show only failed jobs (failed, dead, or completed with non-zero exit code)")
	cmd.Flags().StringVarP(&listStatus, "status", "s", "", "Filter by status (running, paused, starting, pending_placement, completed, queued, dead, failed, processed, unprocessed)")
	cmd.Flags().StringVar(&listHost, "host", "", "Filter by host")
	cmd.Flags().StringVar(&listSince, "since", "", "Show jobs changed since cutoff (YYYY-MM-DD, RFC3339, or duration like \"24h\"/\"7d\")")
	cmd.Flags().BoolVar(&listMine, "mine", false, "Show only jobs from the current project directory")
	cmd.Flags().BoolVar(&listActive, "active", false, "Show only non-terminal jobs")
	cmd.Flags().BoolVar(&listAllHosts, "all-hosts", false, "Include jobs from hosts not synced recently")
	cmd.Flags().StringVar(&listSearch, "search", "", "Search by description or command")
	cmd.Flags().StringVar(&listSearch, "filter", "", "Search by description or command (alias for --search)")
	cmd.Flags().StringSliceVar(&listTags, "tag", nil, "Filter by tag (can be repeated)")
	cmd.Flags().StringSliceVar(&listExcludeTags, "exclude-tag", nil, "Exclude jobs with tag (can be repeated)")
	cmd.Flags().StringVar(&listProject, "project", "", "Filter by project name")
	cmd.Flags().BoolVar(&listProcessed, "processed", false, "Show only jobs with the processed tag")
	// This is the wide unprocessed-history population, including killed and
	// canceled rows. See QuerySessionUnprocessedInbox in specs/job-lifecycle.allium.
	cmd.Flags().BoolVar(&listUnprocessed, "unprocessed", false, "Show only jobs without the processed tag")
	cmd.Flags().BoolVar(&listRental, "rental", false, "Show jobs tagged for rental placement or assigned to rental instances")
	cmd.Flags().BoolVar(&listInventory, "inventory", false, "Show inventory-only jobs and jobs assigned to inventory hosts")
	cmd.Flags().BoolVar(&listCloud, "cloud", false, "Alias for --rental")
	cmd.Flags().MarkHidden("cloud")
	cmd.Flags().IntVar(&listLimit, "limit", 50, "Limit results")
	cmd.Flags().BoolVar(&listSync, "sync", false, "Refresh job statuses before listing")
	cmd.Flags().BoolVar(&listNoSync, "no-sync", false, "Use cached job statuses (default; overrides --sync)")
	cmd.Flags().BoolVarP(&listAll, "all", "a", false, "Include jobs older than 7 days")
	cmd.Flags().StringVar(&listFormat, "format", "table", `Output format: "table", "json", "tsv" (alias: "tab")`)
	cmd.Flags().BoolVar(&listNoTruncate, "no-truncate", false, "Disable column truncation in table mode")
	cmd.Flags().StringSliceVar(&listColumns, "columns", nil, "Columns to display (comma-separated: id, host, status, started, project, dir, description, command, exit_code, duration, tags, gpu)")
}

// addListFlags registers all list-related flags on a command.
// Used by both listCmd and jobListCmd to share the same flag definitions.
func addListFlags(cmd *cobra.Command) {
	addListQueryFlags(cmd)
	cmd.Flags().StringVar(&listShowRaw, "show", "", "Show detailed info for a specific job ID")
	cmd.Flags().IntVar(&listCleanup, "cleanup", 0, "Delete jobs older than N days")
	cmd.Flags().BoolVar(&listTUI, "tui", false, "Force interactive TUI mode")
	cmd.Flags().BoolVar(&listPlain, "plain", false, "Force plain one-shot output")
	cmd.Flags().BoolVarP(&listWatch, "watch", "w", false, "Deprecated alias for --tui")
	cmd.Flags().StringVar(&listGroupBy, "group-by", "", `Group output: "status", "project"`)
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
	cmd.MarkFlagsMutuallyExclusive("watch", "plain")
}

func init() {
	rootCmd.AddCommand(listCmd)
	addListFlags(listCmd)
}

func runList(cmd *cobra.Command, args []string) error {
	var err error
	if listShow, err = parseOptionalJobIDFlag("show", listShowRaw); err != nil {
		return err
	}
	if err := validateListGroupingOptions(); err != nil {
		return err
	}

	openDB := db.OpenForReading
	if listCleanup > 0 {
		openDB = db.Open
	}
	database, err := openDB()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Cleanup and single-job modes need fully-synced state; run the sync
	// synchronously for them.
	if listCleanup > 0 || listShow > 0 {
		printWarnings(syncListDataContext(cmd.Context(), database))
		if listCleanup > 0 {
			deleted, err := db.CleanupOld(database, listCleanup)
			if err != nil {
				return fmt.Errorf("cleanup: %w", err)
			}
			fmt.Printf("Deleted %d jobs older than %d days\n", deleted, listCleanup)
			return nil
		}
		return showJob(database, listShow)
	}

	useTUI, err := resolveListMode()
	if err != nil {
		return err
	}
	if useTUI {
		return runListTUI(cmd, database, args)
	}
	return runListPlain(cmd.Context(), database, args)
}

func runListPlain(ctx context.Context, database *sql.DB, args []string) error {
	if !plainListShouldSync() {
		return renderListPlain(database, args)
	}

	// Render the table from the DB as soon as the sync finishes or a short
	// soft deadline elapses, whichever comes first. The process always
	// blocks on the sync before returning so in-flight DB writes aren't
	// abandoned mid-transaction.
	syncCh := startListSyncAsync(ctx, database)
	const listSyncSoftDeadline = 1 * time.Second
	var (
		syncWarnings []string
		synced       bool
	)
	defer func() {
		if !synced {
			<-syncCh
		}
	}()
	select {
	case syncWarnings = <-syncCh:
		synced = true
	case <-time.After(listSyncSoftDeadline):
	}
	printWarnings(syncWarnings)

	if err := renderListPlain(database, args); err != nil {
		return err
	}

	if !synced {
		clearPendingNotice := writePendingRefreshNotice(os.Stderr, term.IsTerminal(os.Stderr.Fd()))
		late := <-syncCh
		synced = true
		clearPendingNotice()
		printWarnings(dropCloudTimeoutWarnings(late))
	}
	return nil
}

func plainListShouldSync() bool {
	return listSync && !listNoSync
}

func renderListPlain(database *sql.DB, args []string) error {
	listNewestFirst = true
	defer func() { listNewestFirst = false }()
	jobs, err := collectJobsForList(database, args)
	if err != nil {
		return err
	}
	if listOlderHiddenCount > 0 {
		fmt.Fprintf(os.Stderr, "%d older %s hidden by the default %d-day window — pass --all to include them.\n",
			listOlderHiddenCount, pluralize("job", listOlderHiddenCount), defaultListMaxAgeDays)
	}
	if listLimitHiddenCount > 0 {
		fmt.Fprintf(os.Stderr, "%d more %s hidden by --limit %d — pass --limit 0 to include them.\n",
			listLimitHiddenCount, pluralize("job", listLimitHiddenCount), listLimit)
	}
	return printJobsWithSelection(database, jobs, currentJobListJSONSelection())
}

func runListTUI(cmd *cobra.Command, readDB *sql.DB, args []string) error {
	if listFormat != "" && listFormat != "table" {
		return usageErrorf("--tui supports table output only (remove --format or use --plain)")
	}
	if listGroupBy == "project" {
		return usageErrorf("--tui does not support --group-by project")
	}
	ensureDaemonForWork(os.Stderr)

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	jobs, err := collectJobsForList(readDB, args)
	if err != nil {
		return err
	}
	return terminal.RunListTUI(database, args, jobs, buildListTitle(args), !listNoSync, listGroupBy == "status", listProject)
}

func resolveListMode() (bool, error) {
	forceTUI := listTUI || listWatch
	if forceTUI && listPlain {
		return false, usageErrorf("--tui/--watch and --plain are mutually exclusive")
	}
	if listFormat != "" && listFormat != "table" {
		if forceTUI {
			return false, usageErrorf("--tui supports table output only (remove --format or use --plain)")
		}
		return false, nil
	}
	if listGroupBy == "project" {
		if forceTUI {
			return false, usageErrorf("--tui does not support --group-by project")
		}
		return false, nil
	}
	return resolveTUIMode(forceTUI, listPlain, hasTerminalIO(), inAgentContext())
}

func writePendingRefreshNotice(w io.Writer, isTerminal bool) func() {
	msg := degraded.CloudStateStalePendingRefresh()
	if !isTerminal {
		fmt.Fprintln(w, msg)
		return func() {}
	}
	fmt.Fprint(w, msg)
	return func() {
		fmt.Fprint(w, "\r\x1b[2K")
	}
}

// dropCloudTimeoutWarnings filters out the cloud-sync timeout warning, which
// duplicates the stale-pending-refresh notice already shown on this path.
func dropCloudTimeoutWarnings(warnings []string) []string {
	out := warnings[:0]
	for _, w := range warnings {
		if degraded.IsCloudSyncTimeoutWarning(w) {
			continue
		}
		out = append(out, w)
	}
	return out
}

func startListSyncAsync(ctx context.Context, database *sql.DB) <-chan []string {
	ch := make(chan []string, 1)
	go func() { ch <- syncListDataFunc(ctx, database) }()
	return ch
}

var syncListDataFunc = syncListDataContext

func printWarnings(warnings []string) {
	writeWarnings(os.Stderr, warnings)
}

func writeWarnings(w io.Writer, warnings []string) {
	seen := make(map[string]struct{}, len(warnings))
	for _, msg := range warnings {
		if _, ok := seen[msg]; ok {
			continue
		}
		seen[msg] = struct{}{}
		fmt.Fprintln(w, msg)
	}
}

func syncListData(database *sql.DB) []string {
	return syncListDataContext(context.Background(), database)
}

func syncListDataContext(ctx context.Context, database *sql.DB) []string {
	if listNoSync {
		return nil
	}

	var warnings []string
	if listSync {
		if listHost != "" {
			if err := performListSyncForHost(database, listHost); err != nil {
				warnings = append(warnings, fmt.Sprintf("Warning: sync failed: %v", err))
			}
		} else {
			if err := performListSync(database); err != nil {
				warnings = append(warnings, fmt.Sprintf("Warning: sync failed: %v", err))
			}
		}
		warnings = append(warnings, syncSkyForList(ctx, database)...)
		return warnings
	}

	var hostFilter []string
	if listHost != "" {
		hostFilter = []string{listHost}
	}
	result := syncorch.SyncAll(database, nil, syncorch.SyncOptions{
		Hosts:        hostFilter,
		SSHTimeout:   FastSyncTimeout,
		HostTimeout:  FastSyncHostTimeout,
		CloudMode:    syncorch.CloudBounded,
		CloudTimeout: FastCloudSyncTimeout,
	})
	warnings = append(warnings, result.Warnings...)
	warnings = append(warnings, syncSkyForList(ctx, database)...)
	if !result.AllCompleted {
		if note := buildStaleDataNote(database, result.HostsUnreachable, result.HostsSlow); note != "" {
			warnings = append(warnings, note)
		}
	}
	return warnings
}

func syncSkyForList(parent context.Context, database *sql.DB) []string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	_, _, err := syncSkyBindingsAmbient(ctx, database, "")
	if err != nil {
		return []string{fmt.Sprintf("Warning: SkyPilot sync skipped: %v", err)}
	}
	return nil
}

// defaultListMaxAgeDays is the recency window a bare `weft job list` applies:
// jobs that started more than this many days ago are hidden unless --all or
// --since widens the horizon. --limit caps within this window; it does not
// widen it.
const defaultListMaxAgeDays = 7

// listOlderHiddenCount records how many jobs matched the current list filters
// but were hidden by the default recency window. The companion known bit keeps
// a failed count distinct from zero. See JobListMachineSurface in
// specs/job-lifecycle.allium.
var (
	listOlderHiddenCount      int
	listOlderHiddenCountKnown bool
)

// listLimitHiddenCount records how many jobs matched the current list filters
// but were dropped by --limit. Set by collectJobsForList, read by
// renderListPlain, so a capped listing does not read as "this is everything".
var listLimitHiddenCount int

// listMaxAgeDaysApplied records the default recency bound actually used by
// collectJobsForList. See JobListMachineSurface in specs/job-lifecycle.allium.
var listMaxAgeDaysApplied int

// listHostSyncWindowApplied and listHostSyncWindowHiddenCount describe the
// implicit freshness constraint applied by collectJobsForList. See
// JobListMachineSurface in specs/job-lifecycle.allium.
var (
	listHostSyncWindowApplied     bool
	listHostSyncWindowHiddenCount int
)

// listNewestFirst makes collectJobsForList order by job ID descending before
// applying --limit, so the most recently submitted jobs are the ones that
// survive the cap. Only the CLI's plain list path sets it: that surface answers
// "did the job I just submitted get created" for an agent, where recency is the
// useful sort key. The TUI, project listings, and job watch leave it false and
// keep the SQL ordering, which puts active jobs first.
var listNewestFirst bool

// listOrderApplied captures the ordering in force when collectJobsForList
// selects its rows. See JobListMachineSurface in specs/job-lifecycle.allium.
var listOrderApplied = jobListJSONOrderActiveFirst

// sortJobsNewestFirst orders jobs by job ID descending.
func sortJobsNewestFirst(jobs []*db.Job) {
	slices.SortFunc(jobs, func(a, b *db.Job) int { return cmp.Compare(b.ID, a.ID) })
}

// applyListLimit caps a fully filtered job set at --limit. When listNewestFirst
// is set it orders by job ID descending first, so the cap keeps the most
// recently submitted jobs, and it records what the cap dropped in
// listLimitHiddenCount. Every return path in collectJobsForList goes through
// here so no branch can truncate silently.
func applyListLimit(jobs []*db.Job) []*db.Job {
	if listNewestFirst {
		sortJobsNewestFirst(jobs)
	}
	if listLimit > 0 && len(jobs) > listLimit {
		listLimitHiddenCount = len(jobs) - listLimit
		jobs = jobs[:listLimit]
	}
	return jobs
}

func collectJobsForList(database *sql.DB, args []string) ([]*db.Job, error) {
	listOlderHiddenCount = 0
	listOlderHiddenCountKnown = false
	listLimitHiddenCount = 0
	listMaxAgeDaysApplied = 0
	listHostSyncWindowApplied = false
	listHostSyncWindowHiddenCount = 0
	listOrderApplied = jobListJSONOrderActiveFirst
	if listNewestFirst {
		listOrderApplied = jobListJSONOrderNewestFirst
	}
	statusFilter, processedFilter, failedOnly, err := listFilters()
	if err != nil {
		return nil, err
	}
	wantRental, wantInventory, err := listPlacementFlags()
	if err != nil {
		return nil, err
	}

	// Handle explicit job IDs: weft list 12::14, weft list 12...13, weft list 12,13,14
	if len(args) > 0 {
		jobIDs, err := ParseJobIDs(args)
		if err != nil {
			return nil, err
		}
		jobs, missingIDs, err := listJobsByID(database, jobIDs)
		if err != nil {
			return nil, err
		}
		if len(missingIDs) > 0 {
			fmt.Fprintf(os.Stderr, "Warning: job(s) not found: %s\n", ids.FormatJobIDListCompact(missingIDs))
		}
		jobs = filterJobsForListArgs(jobs, statusFilter, processedFilter, failedOnly, wantRental, wantInventory)
		jobs, err = applyPostListFilters(jobs)
		if err != nil {
			return nil, err
		}
		return applyListLimit(jobs), nil
	}

	hostFilterHosts := []string{}
	hostSyncWindowApplies := false
	if listHost != "" {
		hostFilterHosts = []string{listHost}
	} else if listQueued {
		// --queued is an explicit request for all pending jobs; unplaced jobs have no
		// host, so filtering by recently-synced hosts would silently hide them.
	} else if !listAllHosts {
		recentHosts, err := db.ListHostsSyncedSince(database, time.Now().Add(-defaultHostSyncWindow))
		if err != nil {
			return nil, fmt.Errorf("list recent hosts: %w", err)
		}
		if len(recentHosts) > 0 {
			// An empty lookup preserves the collector's existing no-filter behavior;
			// record the constraint only when applied. See JobListMachineSurface.
			hostFilterHosts = recentHosts
			hostSyncWindowApplies = true
			listHostSyncWindowApplied = true
		}
	}

	// Handle search
	if listSearch != "" {
		searchLimit := listLimit
		if listNewestFirst || len(listTags) > 0 || processedFilter != "" || failedOnly || len(listExcludeTags) > 0 || len(hostFilterHosts) > 0 || listProject != "" || listMine || wantRental || wantInventory || listSince != "" || listActive {
			searchLimit = 0
		}
		jobs, err := db.SearchJobs(database, listSearch, searchLimit)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		jobs = jobsWithEffectiveStatus(jobs, statusFilter)
		jobs = filterJobsByFailureState(jobs, failedOnly)
		jobs = db.FilterJobsByTags(jobs, listTags, processedFilter)
		jobs = filterJobsByHostFlag(jobs)
		jobs = db.FilterJobsByExcludedTags(jobs, listExcludeTags)
		jobs = db.FilterJobsByProject(jobs, listProject)
		jobs = filterJobsByMine(jobs)
		jobs = filterJobsByPlacementScope(jobs, wantRental, wantInventory)
		jobs, err = applyPostListFilters(jobs)
		if err != nil {
			return nil, err
		}
		if hostSyncWindowApplies {
			beforeHostWindow := len(jobs)
			jobs = db.FilterByFreshStatus(jobs, hostFilterHosts)
			listHostSyncWindowHiddenCount = beforeHostWindow - len(jobs)
		}
		return applyListLimit(jobs), nil
	}

	// Default to the recency window unless --all or --since widens it.
	maxAgeDays := defaultListMaxAgeDays
	if listAll || listSince != "" {
		maxAgeDays = 0
	}
	listMaxAgeDaysApplied = maxAgeDays

	// applyGoFilters runs the in-Go filter chain that the SQL query cannot
	// express. Sharing it between the display fetch and the hidden-older count
	// keeps the two counts comparable across the window boundary.
	applyGoFilters := func(js []*db.Job) ([]*db.Job, error) {
		js = filterJobsByHostFlag(js)
		js = jobsWithEffectiveStatus(js, statusFilter)
		js = filterJobsByFailureState(js, failedOnly)
		js = db.FilterJobsByExcludedTags(js, listExcludeTags)
		js = db.FilterJobsByProject(js, listProject)
		js = filterJobsByMine(js)
		js = filterJobsByPlacementScope(js, wantRental, wantInventory)
		return applyPostListFilters(js)
	}

	// Fetch the window unbounded (the window itself already bounds the set to
	// recent jobs); --limit is applied last, after the filters. Bounding the
	// SQL query by --limit here would let filters drop rows and silently
	// under-fill the result below the requested limit.
	raw, err := db.ListJobsWithMaxAge(database, statusFilter, "", 0, maxAgeDays, listTags, processedFilter)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	jobs, err := applyGoFilters(raw)
	if err != nil {
		return nil, err
	}
	if hostSyncWindowApplies {
		beforeHostWindow := len(jobs)
		jobs = db.FilterByFreshStatus(jobs, hostFilterHosts)
		listHostSyncWindowHiddenCount = beforeHostWindow - len(jobs)
	}

	// When the recency window is active, jobs that started before it are hidden
	// and --limit will never reveal them. Count the hidden matches so the caller
	// can say so — otherwise the cutoff reads as "these jobs were deleted".
	if maxAgeDays > 0 {
		allRaw, allErr := db.ListJobsWithMaxAge(database, statusFilter, "", 0, 0, listTags, processedFilter)
		if allErr == nil {
			all, filterErr := applyGoFilters(allRaw)
			if filterErr == nil {
				if hostSyncWindowApplies {
					all = db.FilterByFreshStatus(all, hostFilterHosts)
				}
				listOlderHiddenCountKnown = true
				if len(all) > len(jobs) {
					listOlderHiddenCount = len(all) - len(jobs)
				}
			}
		}
	}

	return applyListLimit(jobs), nil
}

func collectJobsForListWithFilters(database *sql.DB, args []string, statusFilter, processedFilter, projectFilter string) ([]*db.Job, error) {
	previousStatus := listStatus
	previousProcessed := listProcessed
	previousUnprocessed := listUnprocessed
	previousRunning := listRunning
	previousCompleted := listCompleted
	previousQueued := listQueued
	previousDead := listDead
	previousProject := listProject
	defer func() {
		listStatus = previousStatus
		listProcessed = previousProcessed
		listUnprocessed = previousUnprocessed
		listRunning = previousRunning
		listCompleted = previousCompleted
		listQueued = previousQueued
		listDead = previousDead
		listProject = previousProject
	}()

	if statusFilter != "" {
		listStatus = statusFilter
		listRunning = false
		listCompleted = false
		listQueued = false
		listDead = false
	}
	listProcessed = false
	listUnprocessed = false
	switch processedFilter {
	case "processed":
		listProcessed = true
	case "unprocessed":
		listUnprocessed = true
	}
	listProject = projectFilter
	return collectJobsForList(database, args)
}

func applyPostListFilters(jobs []*db.Job) ([]*db.Job, error) {
	var err error
	if listSince != "" {
		var cutoff time.Time
		cutoff, err = parseSinceCutoff(listSince, time.Now())
		if err != nil {
			return nil, err
		}
		jobs = filterJobsSince(jobs, cutoff)
	}
	if listActive {
		jobs = filterActiveJobs(jobs)
	}
	return jobs, nil
}

func filterJobsSince(jobs []*db.Job, cutoff time.Time) []*db.Job {
	cutoffUnix := cutoff.Unix()
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if latestJobTimestamp(job) >= cutoffUnix {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func latestJobTimestamp(job *db.Job) int64 {
	latest := job.CreatedAt
	if job.QueuedAt > latest {
		latest = job.QueuedAt
	}
	if job.StartTime > latest {
		latest = job.StartTime
	}
	if job.EndTime != nil && *job.EndTime > latest {
		latest = *job.EndTime
	}
	return latest
}

func filterActiveJobs(jobs []*db.Job) []*db.Job {
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil || job.Tombstoned {
			continue
		}
		switch job.EffectiveStatus() {
		case db.StatusCompleted, db.StatusFailed, db.StatusDead, db.StatusKilled, db.StatusCanceled:
			continue
		default:
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func listFilters() (statusFilter, processedFilter string, failedOnly bool, err error) {
	if listStatus == "processed" || listStatus == "unprocessed" {
		processedFilter = listStatus
	}
	if listProcessed {
		if processedFilter == "unprocessed" {
			return "", "", false, usageErrorf("--processed and --unprocessed are mutually exclusive")
		}
		processedFilter = "processed"
	}
	if listUnprocessed {
		if processedFilter == "processed" {
			return "", "", false, usageErrorf("--processed and --unprocessed are mutually exclusive")
		}
		processedFilter = "unprocessed"
	}
	if listStatus != "" && listStatus != "processed" && listStatus != "unprocessed" {
		statusFilter = listStatus
	}
	if listRunning {
		statusFilter = db.StatusRunning
	} else if listCompleted {
		statusFilter = db.StatusCompleted
	} else if listQueued {
		statusFilter = db.StatusQueued
	} else if listDead {
		statusFilter = db.StatusDead
	}
	return statusFilter, processedFilter, listFailed, nil
}

func listJobsByID(database *sql.DB, jobIDs []int64) ([]*db.Job, []int64, error) {
	jobs := make([]*db.Job, 0, len(jobIDs))
	missing := make([]int64, 0)
	for _, id := range jobIDs {
		job, err := db.GetJobByID(database, id)
		if err != nil {
			return nil, nil, fmt.Errorf("get job %s: %w", ids.FormatJobID(id), err)
		}
		if job == nil || job.Tombstoned {
			missing = append(missing, id)
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, missing, nil
}

func filterJobsForListArgs(jobs []*db.Job, statusFilter, processedFilter string, failedOnly, wantRental, wantInventory bool) []*db.Job {
	jobs = jobsWithEffectiveStatus(jobs, statusFilter)
	jobs = filterJobsByFailureState(jobs, failedOnly)

	if listSearch != "" {
		filter := strings.ToLower(listSearch)
		filtered := make([]*db.Job, 0, len(jobs))
		for _, job := range jobs {
			if strings.Contains(strings.ToLower(job.EffectiveDescription()), filter) ||
				strings.Contains(strings.ToLower(job.EffectiveCommand()), filter) {
				filtered = append(filtered, job)
			}
		}
		jobs = filtered
	}

	jobs = db.FilterJobsByTags(jobs, listTags, processedFilter)
	jobs = db.FilterJobsByExcludedTags(jobs, listExcludeTags)
	jobs = db.FilterJobsByProject(jobs, listProject)
	jobs = filterJobsByMine(jobs)
	jobs = filterJobsByPlacementScope(jobs, wantRental, wantInventory)
	jobs = filterJobsByHostFlag(jobs)
	return jobs
}

func listPlacementFlags() (wantRental, wantInventory bool, err error) {
	wantRental = listRental || listCloud
	wantInventory = listInventory
	if wantRental && wantInventory {
		return false, false, usageErrorf("--rental and --inventory are mutually exclusive")
	}
	return wantRental, wantInventory, nil
}

func filterJobsByPlacementScope(jobs []*db.Job, wantRental, wantInventory bool) []*db.Job {
	if !wantRental && !wantInventory {
		return jobs
	}
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		switch {
		case wantRental && job.UsesRentalPlacement():
			filtered = append(filtered, job)
		case wantInventory && job.UsesInventoryPlacement():
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func filterJobsByHostFlag(jobs []*db.Job) []*db.Job {
	if listHost == "" {
		return jobs
	}
	return db.FilterByHost(jobs, []string{listHost})
}

func filterJobsByMine(jobs []*db.Job) []*db.Job {
	if !listMine {
		return jobs
	}
	wd, err := os.Getwd()
	if err != nil {
		return jobs
	}
	project := filepath.Base(wd)
	return db.FilterJobsByProject(jobs, project)
}

func filterJobsByFailureState(jobs []*db.Job, failedOnly bool) []*db.Job {
	if !failedOnly {
		return jobs
	}
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if isFailedJob(job) {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func isFailedJob(job *db.Job) bool {
	if job == nil {
		return false
	}
	switch job.EffectiveStatus() {
	case db.StatusFailed, db.StatusDead:
		return true
	case db.StatusCompleted:
		return job.ExitCode != nil && *job.ExitCode != 0
	default:
		return false
	}
}

func showJob(database *sql.DB, id int64) error {
	job, err := db.GetJobByID(database, id)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %s not found", ids.FormatJobID(id))
	}

	fmt.Printf("Job ID:       %s\n", ids.FormatJobID(job.ID))
	fmt.Printf("Target:       %s\n", job.TargetDisplay())
	fmt.Printf("Working Dir:  %s\n", job.EffectiveWorkingDir())
	if job.Project != "" {
		fmt.Printf("Project:      %s\n", job.Project)
	}
	fmt.Printf("Command:      %s\n", job.EffectiveCommand())
	if job.Description != "" {
		fmt.Printf("Description:  %s\n", job.Description)
	} else if job.GeneratedDescription != "" {
		fmt.Printf("Description:  %s (AI-generated)\n", job.GeneratedDescription)
	}
	if tags := job.DisplayTags(); len(tags) > 0 {
		fmt.Printf("Tags:         %s\n", strings.Join(tags, ", "))
	}
	if len(job.Inputs) > 0 {
		fmt.Printf("Inputs:       %s\n", strings.Join(job.Inputs, ", "))
		inputPaths, err := namedAssetInputPaths(database, job.Inputs)
		if err != nil {
			return err
		}
		if len(inputPaths) > 0 {
			fmt.Printf("Input Paths:  %s\n", strings.Join(inputPaths, ", "))
		}
	}
	if len(job.BestEffortInputs) > 0 {
		fmt.Printf("Best Effort:  %s\n", strings.Join(job.BestEffortInputs, ", "))
	}
	fmt.Printf("Status:       %s\n", job.EffectiveStatus())
	printExternalBindingSummary(database, job, "External:     ", "              ")
	fmt.Printf("Start Time:   %s\n", util.FormatUnixTimeOr(job.StartTime, "2006-01-02 15:04:05", util.EmptyCellCLI))
	if job.EndTime != nil {
		fmt.Printf("End Time:     %s\n", time.Unix(*job.EndTime, 0).Format("2006-01-02 15:04:05"))
		if job.StartTime > 0 && *job.EndTime >= job.StartTime {
			duration := *job.EndTime - job.StartTime
			fmt.Printf("Duration:     %s\n", db.FormatDuration(duration))
		}
	}
	if job.ExitCode != nil {
		fmt.Printf("Exit Code:    %d\n", *job.ExitCode)
	}
	printUploadFailureForJob(database, job)
	if progress := jobProgressSummary(database, job); progress != "" {
		fmt.Printf("Progress:     %s\n", progress)
	}
	printJobLocalDiagnostics(database, job)
	printJobMoveSummary(database, job)
	printJobAttemptSummary(database, job)

	return nil
}

func namedAssetInputPaths(database *sql.DB, inputs []string) ([]string, error) {
	paths := make([]string, 0, len(inputs))
	for _, input := range inputs {
		asset, ok := dataloc.ParseAssetRef(input)
		if !ok || asset.Kind != dataloc.AssetNamed {
			continue
		}
		named, err := db.GetNamedAssetByName(database, asset.ID)
		if errors.Is(err, db.ErrNamedAssetNotFound) {
			paths = append(paths, input+" -> unavailable (not published)")
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve input %s target path: %w", input, err)
		}
		paths = append(paths, input+" -> "+named.TargetPath)
	}
	return paths, nil
}

func printExternalBindingSummary(database *sql.DB, job *db.Job, firstPrefix, nextPrefix string) {
	if database == nil || job == nil || job.Backend != db.BackendSkyPilot {
		return
	}
	binding, err := db.GetExternalJobBindingByJobID(database, job.ID)
	if err != nil {
		fmt.Printf("%sSkyPilot (binding unavailable)\n", firstPrefix)
		return
	}
	for i, line := range externalBindingSummaryLines(job, binding, time.Now()) {
		prefix := nextPrefix
		if i == 0 {
			prefix = firstPrefix
		}
		fmt.Printf("%s%s\n", prefix, line)
	}
}

const externalBindingStaleAfter = 10 * time.Minute

func externalBindingSummaryLines(job *db.Job, binding *db.ExternalJobBinding, now time.Time) []string {
	if binding == nil {
		if db.IsExternalSubmissionUnconfirmed(job, nil, now) {
			return []string{
				"SkyPilot submission unconfirmed; external work may still be running or billing",
				"Recover: weft sky import --job " + ids.FormatJobID(job.ID) + " <sky-job-id>",
			}
		}
		return []string{"SkyPilot (binding unavailable)"}
	}
	parts := []string{"SkyPilot"}
	if binding.ExternalJobID != "" {
		parts = append(parts, "job "+binding.ExternalJobID)
	}
	if binding.ExternalTaskID != "" {
		parts = append(parts, "task "+binding.ExternalTaskID)
	}
	if binding.ExternalClusterName != "" {
		parts = append(parts, "cluster "+binding.ExternalClusterName)
	}
	if binding.RawStatus != "" {
		parts = append(parts, "raw "+binding.RawStatus)
	}
	lines := []string{strings.Join(parts, " · ")}
	if binding.CancelRequestedAt != nil {
		lines = append(lines, "SkyPilot cancel requested; awaiting terminal confirmation")
	}
	if binding.RawStatusMessage != "" {
		lines = append(lines, binding.RawStatusMessage)
	}
	// The executor's message and weft's own observation failure are separate
	// channels: the status above is only as good as its last observation, and
	// a stale one must not read as current.
	if binding.SyncWarning != "" {
		age := ""
		if binding.LastObservedAt != nil {
			age = fmt.Sprintf(" (status last confirmed %s ago)",
				db.FormatDuration(now.Unix()-*binding.LastObservedAt))
		}
		lines = append(lines, fmt.Sprintf("weft could not refresh this status%s: %s", age, binding.SyncWarning))
	} else if binding.LastObservedAt == nil {
		lines = append(lines, "weft has never confirmed this status")
	} else if age := now.Sub(time.Unix(*binding.LastObservedAt, 0)); age > externalBindingStaleAfter {
		lines = append(lines, fmt.Sprintf("weft status observation is stale (last confirmed %s ago)", db.FormatDuration(int64(age.Seconds()))))
	}
	if binding.DashboardURL != "" {
		lines = append(lines, binding.DashboardURL)
	}
	return lines
}

func printJobMoveSummary(database *sql.DB, job *db.Job) {
	statusByJob, err := jobview.PlacementStatusForJobs(database, []*db.Job{job}, time.Now())
	if err != nil {
		return
	}
	move := statusByJob[job.ID].Move
	if move == nil {
		return
	}
	parts := []string{jobview.FormatMovePath(move.SourceLabel, move.TargetLabel)}
	if strings.TrimSpace(move.Phase) != "" {
		parts = append(parts, strings.TrimSpace(move.Phase))
	}
	if move.State == db.MoveIntentStateOpen && move.CreatedAt > 0 {
		parts = append(parts, "pending "+db.FormatDuration(time.Now().Unix()-move.CreatedAt))
	}
	if move.State != db.MoveIntentStateOpen && move.ResolvedAt != nil {
		parts = append(parts, "resolved "+db.FormatDuration(time.Now().Unix()-*move.ResolvedAt)+" ago")
	}
	if move.State != db.MoveIntentStateOpen && strings.TrimSpace(move.Resolution) != "" {
		parts = append(parts, strings.TrimSpace(move.Resolution))
	}
	fmt.Printf("Move:         %s\n", strings.Join(parts, " · "))
}

func printJobAttemptSummary(database *sql.DB, job *db.Job) {
	attempts, err := db.ListAttempts(database, job.ID)
	if err != nil || len(attempts) == 0 {
		return
	}
	latest := attempts[0]
	if latest.StartTime != nil {
		fmt.Printf("Latest:       #%d started %s on %s\n", latest.AttemptNumber, formatUnixTime(*latest.StartTime), attemptTarget(latest))
	}
	if previous := previousInstanceList(attempts); previous != "" {
		fmt.Printf("Previous:     %s\n", previous)
	}
}

// printJobs renders with the selection bounds of the collection in flight. Every
// JSON caller carries one; there is no unenveloped shape. See
// JobListMachineSurface in specs/job-lifecycle.allium.
func printJobs(database *sql.DB, jobs []*db.Job) error {
	return printJobsWithSelection(database, jobs, currentJobListJSONSelection())
}

func printJobsWithSelection(database *sql.DB, jobs []*db.Job, selection jobListJSONSelection) error {
	applyAttemptOutcomeOverrides(database, jobs)
	queueblock.Apply(jobs, queueblock.Fetch(jobs, 5*time.Second))

	if listGroupBy == "status" {
		if groupedUnprocessedViewExcludesCanceled() {
			jobs = excludeStatusForGroupedView(jobs, db.StatusCanceled)
		}
		liveByLaunchID := loadLaunchLiveStateForJobs(database, jobs)
		launchStatusByID := loadLaunchStatusForJobs(database, jobs)
		placementStatusByJob, err := jobview.PlacementStatusForJobs(database, jobs, time.Now())
		if err != nil {
			placementStatusByJob = map[int64]jobview.PlacementStatus{}
		}
		return terminal.WriteListPlainOutput(terminal.RenderJobListGroupedStatusPlainWithOptions(database, jobs, terminal.ListOutputWidth(), liveByLaunchID, launchStatusByID, placementStatusByJob))
	}
	if listGroupBy == "project" {
		return terminal.WriteListPlainOutput(terminal.RenderProjectJobsPlain(terminal.GroupJobsByProject(jobs), terminal.ListOutputWidth()))
	}

	switch listFormat {
	case "json":
		cols, err := terminal.ResolveColumns(listColumns, terminal.DefaultJSONColumnKeys)
		if err != nil {
			return err
		}
		if err := populateSubmitterSessionColumn(database, jobs, terminal.DefaultJSONColumnKeys); err != nil {
			return err
		}
		return writeJobListJSON(os.Stdout, jobs, cols, selection)
	case "tsv", "tab":
		cols, err := terminal.ResolveColumns(listColumns, terminal.DefaultTSVColumnKeys)
		if err != nil {
			return err
		}
		if err := populateSubmitterSessionColumn(database, jobs, terminal.DefaultTSVColumnKeys); err != nil {
			return err
		}
		return terminal.PrintJobsTSV(os.Stdout, jobs, cols)
	case "table", "":
		// No table layout lists the column by default, so only an explicit
		// --columns can ask for it.
		if err := populateSubmitterSessionColumn(database, jobs, nil); err != nil {
			return err
		}
		return terminal.WriteListPlainOutput(terminal.RenderJobListPlainWithOptions(jobs, terminal.ListOutputWidth(), listColumns, listNoTruncate))
	default:
		return fmt.Errorf("unknown format %q (use table, json, or tsv)", listFormat)
	}
}

const jobListJSONVersion = 1

type jobListJSONConstraintKind string

const (
	jobListJSONConstraintMaxAgeDays     jobListJSONConstraintKind = "max_age_days"
	jobListJSONConstraintLimit          jobListJSONConstraintKind = "limit"
	jobListJSONConstraintHostSyncWindow jobListJSONConstraintKind = "host_sync_window"
)

func (kind jobListJSONConstraintKind) valid() bool {
	switch kind {
	case jobListJSONConstraintMaxAgeDays, jobListJSONConstraintLimit, jobListJSONConstraintHostSyncWindow:
		return true
	default:
		return false
	}
}

type jobListJSONOrder string

const (
	jobListJSONOrderNewestFirst jobListJSONOrder = "newest_first"
	jobListJSONOrderActiveFirst jobListJSONOrder = "active_first"
)

func (order jobListJSONOrder) valid() bool {
	return order == jobListJSONOrderNewestFirst || order == jobListJSONOrderActiveFirst
}

// jobListJSONEnvelope is the versioned job-list machine surface. Selection is
// never omitted. See JobListMachineSurface in specs/job-lifecycle.allium.
type jobListJSONEnvelope struct {
	Kind      string               `json:"kind"`
	Version   int                  `json:"version"`
	Selection jobListJSONSelection `json:"selection"`
	Jobs      json.RawMessage      `json:"jobs"`
}

type jobListJSONConstraint struct {
	Kind      jobListJSONConstraintKind `json:"kind"`
	Value     *int                      `json:"value"`
	Hidden    *int                      `json:"hidden"`
	Requested bool                      `json:"requested"`
}

type jobListJSONSelection struct {
	Complete    bool                    `json:"complete"`
	Order       jobListJSONOrder        `json:"order"`
	Constraints []jobListJSONConstraint `json:"constraints"`
}

func currentJobListJSONSelection() jobListJSONSelection {
	constraints := make([]jobListJSONConstraint, 0, 3)
	if listMaxAgeDaysApplied > 0 {
		constraint := newJobListJSONConstraint(
			jobListJSONConstraintMaxAgeDays, listMaxAgeDaysApplied, listOlderHiddenCount, true,
		)
		if !listOlderHiddenCountKnown {
			constraint.Hidden = nil
		}
		constraints = append(constraints, constraint)
	}
	if listLimit > 0 {
		constraints = append(constraints, newJobListJSONConstraint(
			jobListJSONConstraintLimit, listLimit, listLimitHiddenCount, true,
		))
	}
	if listHostSyncWindowApplied {
		constraints = append(constraints, newJobListJSONConstraint(
			jobListJSONConstraintHostSyncWindow,
			int(defaultHostSyncWindow/time.Second),
			listHostSyncWindowHiddenCount,
			false,
		))
	}
	return newJobListJSONSelection(listOrderApplied, constraints)
}

func newJobListJSONConstraint(kind jobListJSONConstraintKind, value, hidden int, requested bool) jobListJSONConstraint {
	return jobListJSONConstraint{Kind: kind, Value: &value, Hidden: &hidden, Requested: requested}
}

// newJobListJSONSelection derives completeness from every applied constraint;
// adding a constraint cannot leave Complete stale. See JobListMachineSurface
// in specs/job-lifecycle.allium.
func newJobListJSONSelection(order jobListJSONOrder, constraints []jobListJSONConstraint) jobListJSONSelection {
	if constraints == nil {
		constraints = []jobListJSONConstraint{}
	}
	complete := true
	for _, constraint := range constraints {
		if constraint.Hidden == nil || *constraint.Hidden != 0 {
			complete = false
		}
	}
	return jobListJSONSelection{Complete: complete, Order: order, Constraints: constraints}
}

func (selection jobListJSONSelection) validate() error {
	if !selection.Order.valid() {
		return fmt.Errorf("invalid job-list order %q", selection.Order)
	}
	complete := true
	seen := make(map[jobListJSONConstraintKind]struct{}, len(selection.Constraints))
	for _, constraint := range selection.Constraints {
		if !constraint.Kind.valid() {
			return fmt.Errorf("invalid job-list constraint kind %q", constraint.Kind)
		}
		if _, ok := seen[constraint.Kind]; ok {
			return fmt.Errorf("duplicate job-list constraint kind %q", constraint.Kind)
		}
		seen[constraint.Kind] = struct{}{}
		if constraint.Value == nil {
			return fmt.Errorf("job-list constraint %q has no value", constraint.Kind)
		}
		if constraint.Requested != (constraint.Kind != jobListJSONConstraintHostSyncWindow) {
			return fmt.Errorf("job-list constraint %q has invalid requested value", constraint.Kind)
		}
		if constraint.Hidden == nil || *constraint.Hidden != 0 {
			complete = false
		}
	}
	if selection.Complete != complete {
		return fmt.Errorf("job-list selection complete is not derived from its constraints")
	}
	return nil
}

func writeJobListJSON(w io.Writer, jobs []*db.Job, cols []terminal.ColumnDef, selection jobListJSONSelection) error {
	if err := selection.validate(); err != nil {
		return err
	}
	var rows bytes.Buffer
	if err := terminal.PrintJobsJSON(&rows, jobs, cols); err != nil {
		return fmt.Errorf("encode job-list rows: %w", err)
	}
	envelope := jobListJSONEnvelope{
		Kind:      "job_list",
		Version:   jobListJSONVersion,
		Selection: selection,
		Jobs:      json.RawMessage(bytes.TrimSpace(rows.Bytes())),
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(envelope)
}

// populateSubmitterSessionColumn fills in the submitter session when the
// selected columns ask for it. The value is stored on the jobs table but is not
// projected by the views job rows are read through, so it takes a second query
// that is worth skipping when nothing renders it. defaultKeys mirrors the
// fallback in terminal.ResolveColumns: an empty --columns selects them.
func populateSubmitterSessionColumn(database *sql.DB, jobs []*db.Job, defaultKeys []string) error {
	keys := listColumns
	if len(keys) == 0 {
		keys = defaultKeys
	}
	if !slices.Contains(keys, "submitter_session") {
		return nil
	}
	return db.PopulateSubmitterSessions(database, jobs)
}

func groupedUnprocessedViewExcludesCanceled() bool {
	_, processedFilter, _, err := listFilters()
	return err == nil && processedFilter == "unprocessed"
}

func excludeStatusForGroupedView(jobs []*db.Job, status string) []*db.Job {
	if len(jobs) == 0 {
		return jobs
	}
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job != nil && job.EffectiveStatus() == status {
			continue
		}
		filtered = append(filtered, job)
	}
	return filtered
}

func validateListGroupingOptions() error {
	if listGroupBy == "" {
		return nil
	}
	if listGroupBy != "status" && listGroupBy != "project" {
		return usageErrorf("unknown value %q for --group-by (supported: status, project)", listGroupBy)
	}
	if listFormat != "" && listFormat != "table" {
		return usageErrorf("--group-by %s supports table output only (remove --format or use --format table)", listGroupBy)
	}
	return nil
}

func loadLaunchLiveStateForJobs(database *sql.DB, jobs []*db.Job) map[int64]*db.LaunchLiveState {
	if database == nil || len(jobs) == 0 {
		return nil
	}
	launchIDs := make(map[int64]struct{})
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		launchIDs[*job.LaunchID] = struct{}{}
	}
	if len(launchIDs) == 0 {
		return nil
	}
	liveByLaunchID := make(map[int64]*db.LaunchLiveState, len(launchIDs))
	for launchID := range launchIDs {
		live, err := db.GetLaunchLiveState(database, launchID)
		if err != nil || live == nil {
			continue
		}
		liveByLaunchID[launchID] = live
	}
	if len(liveByLaunchID) == 0 {
		return nil
	}
	return liveByLaunchID
}

func loadLaunchStatusForJobs(database *sql.DB, jobs []*db.Job) map[int64]string {
	if database == nil || len(jobs) == 0 {
		return nil
	}
	launchIDs := make([]int64, 0, len(jobs))
	seen := make(map[int64]struct{}, len(jobs))
	for _, job := range jobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
			continue
		}
		if _, ok := seen[*job.LaunchID]; ok {
			continue
		}
		seen[*job.LaunchID] = struct{}{}
		launchIDs = append(launchIDs, *job.LaunchID)
	}
	if len(launchIDs) == 0 {
		return nil
	}
	statusByID, err := db.GetLaunchStatuses(database, launchIDs)
	if err != nil || len(statusByID) == 0 {
		return nil
	}
	return statusByID
}

// applyAttemptOutcomeOverrides overrides the display status for queued jobs
// whose latest cloud attempt outcome is "failed". These are jobs that ran
// and failed on a cloud instance, then were reset to queued — they should
// display as "failed" rather than "queued".
func applyAttemptOutcomeOverrides(database *sql.DB, jobs []*db.Job) {
	if database == nil {
		return
	}
	for _, job := range jobs {
		if job == nil || job.Status != db.StatusQueued {
			continue
		}
		outcome := db.GetLatestAttemptOutcome(database, job.ID)
		if outcome == db.AttemptOutcomeFailed {
			job.Status = db.StatusFailed
		}
	}
}

func buildListTitle(args []string) string {
	parts := []string{"Jobs"}
	if len(args) > 0 {
		parts = append(parts, "selection")
	}
	if listHost != "" {
		parts = append(parts, "host="+listHost)
	}
	status, processed, failedOnly, err := listFilters()
	if err != nil {
		return "Jobs"
	}
	if status != "" {
		parts = append(parts, "status="+status)
	}
	if processed != "" {
		parts = append(parts, processed)
	}
	wantRental, wantInventory, err := listPlacementFlags()
	if err == nil {
		if wantRental {
			parts = append(parts, db.TagRental)
		}
		if wantInventory {
			parts = append(parts, db.TagInventory)
		}
	}
	if failedOnly {
		parts = append(parts, "failed")
	}
	if status == "" && processed == "" && !failedOnly {
		if listQueued {
			parts = append(parts, "queued")
		}
		if listRunning {
			parts = append(parts, "running")
		}
		if listCompleted {
			parts = append(parts, "completed")
		}
		if listDead {
			parts = append(parts, "dead")
		}
	}
	if listSearch != "" {
		parts = append(parts, "search="+listSearch)
	}
	if listSince != "" {
		parts = append(parts, "since="+listSince)
	}
	if listMine {
		parts = append(parts, "mine")
	}
	if listProject != "" {
		parts = append(parts, "project="+listProject)
	}
	if listActive {
		parts = append(parts, "active")
	}
	return strings.Join(parts, " • ")
}

// performListSync runs sync for list --sync flag
func performListSync(database *sql.DB) error {
	hosts, err := db.ListUniqueRunningHosts(database)
	if err != nil {
		return err
	}

	if len(hosts) == 0 {
		return nil
	}

	var updated int
	for _, host := range hosts {
		result, err := syncHost(database, host)
		if err != nil {
			// Silently skip connection errors, warn on others
			if !ssh.IsConnectionError(err.Error()) {
				fmt.Fprintf(os.Stderr, "Warning: error syncing %s: %v\n", host, err)
			}
			continue
		}
		updated += result.Updated
	}

	if updated > 0 {
		fmt.Printf("(synced %d job status(es))\n", updated)
	}

	return nil
}

// performListSyncForHost runs sync for a single host (used with --host flag)
func performListSyncForHost(database *sql.DB, host string) error {
	result, err := syncHost(database, host)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return nil // Silently skip connection errors
		}
		return err
	}

	if result.Updated > 0 {
		fmt.Printf("(synced %d job status(es))\n", result.Updated)
	}

	return nil
}

// startQueueRunnersForQueuedHosts starts queue runners on hosts that have queue-runner jobs.
func startQueueRunnersForQueuedHosts(database *sql.DB) {
	hosts, err := db.ListHostsWithQueueRunnerJobs(database)
	if err != nil {
		return // Silently ignore errors
	}

	startQueueRunnersForHosts(database, hosts)
}

func startQueueRunnersForHosts(database *sql.DB, hosts []string) {
	hosts = uniqueHosts(hosts)
	if len(hosts) == 0 {
		return
	}
	for _, host := range hosts {
		count, err := db.CountQueueRunnerActiveByHost(database, host)
		if err != nil || count == 0 {
			continue
		}
		_, err = ensureQueueRunnerStarted(host)
		if err != nil {
			slog.Warn("could not start queue runner", "host", host, "error", err)
			continue
		}
	}
}
