package cmd

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2resolve"
	"github.com/osteele/weft/internal/runner"
	"github.com/osteele/weft/internal/ssh"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
	"github.com/spf13/cobra"
)

var artifactCmd = &cobra.Command{
	Use:     "artifact",
	Aliases: []string{"artifacts"},
	Short:   "Manage job artifacts",
	Long: `Manage job artifacts produced by jobs.

Artifacts are declared by writing a manifest on the remote host and then
synced into a durable local store for retrieval.`,
	// Error on an unknown subcommand (e.g. `weft artifact lsst`) instead of
	// silently printing this command's help, which reads like success.
	RunE: runArtifactRoot,
}

// runArtifactRoot shows help for a bare `weft artifact` and rejects an unknown
// subcommand token with an actionable error and a suggestion.
func runArtifactRoot(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	return unknownSubcommandError(cmd, args[0])
}

// unknownSubcommandError builds a usage-style error naming the bad subcommand
// and, when cobra can find a near match, suggesting it.
func unknownSubcommandError(parent *cobra.Command, name string) error {
	msg := fmt.Sprintf("unknown subcommand %q for %q", name, parent.CommandPath())
	if suggestions := parent.SuggestionsFor(name); len(suggestions) > 0 {
		quoted := make([]string, len(suggestions))
		for i, s := range suggestions {
			quoted[i] = strconv.Quote(s)
		}
		msg += "; did you mean " + strings.Join(quoted, " or ") + "?"
	}
	return usageErrorf("%s", msg)
}

var artifactSyncCmd = &cobra.Command{
	Use:   "sync [job-id]...",
	Short: "Sync artifacts into the local store",
	Args:  usageArgs(cobra.MinimumNArgs(0)),
	RunE:  runArtifactSync,
}

var artifactListCmd = &cobra.Command{
	Use:     "list <job-id>...",
	Aliases: []string{"ls"},
	Short:   "List cached artifacts for a job",
	Args:    usageArgs(cobra.MinimumNArgs(1)),
	RunE:    runArtifactList,
}

var artifactGetCmd = &cobra.Command{
	Use:   "get <job-id>... <name-or-path>",
	Short: "Retrieve an artifact or output file",
	Long: `Retrieve an artifact or output file from the local cache or cloud storage.

Use --all to download all artifacts for a job.
Use --tag with --latest to resolve the job ID from tags.`,
	Args: usageArgs(func(cmd *cobra.Command, args []string) error {
		if artifactAll {
			if len(artifactTag) > 0 {
				if !artifactLatest {
					return usageErrorf("use --latest when resolving by tag")
				}
				return cobra.ExactArgs(0)(cmd, args)
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		}
		if len(artifactTag) > 0 {
			if !artifactLatest {
				return usageErrorf("use --latest when resolving by tag")
			}
			if len(args) != 1 {
				return usageErrorf("expected 1 arg (<name-or-path>) when using --tag")
			}
			return nil
		}
		return cobra.MinimumNArgs(2)(cmd, args)
	}),
	RunE: runArtifactGet,
}

var artifactAddCmd = &cobra.Command{
	Use:   "add <job-id> <path>",
	Short: "Add an artifact entry to the remote manifest",
	Args:  usageArgs(cobra.ExactArgs(2)),
	RunE:  runArtifactAdd,
}

var artifactPruneLocalCmd = &cobra.Command{
	Use:   "prune-local",
	Short: "Delete local files that are restorable via weft commands",
	Long: `Delete local files that can be restored via weft artifact/sync workflows.

By default this command runs in dry-run mode and only prints what would be
deleted. Pass --apply to actually delete files.`,
	Args: usageArgs(cobra.NoArgs),
	RunE: runArtifactPruneLocal,
}

var artifactCatCmd = &cobra.Command{
	Use:   "cat <job-id> <name-or-path>",
	Short: "Write an artifact or output file to stdout",
	Long: `Write an artifact or output file from the local cache or cloud storage to stdout.

Use --tag with --latest to resolve the job ID from tags.`,
	Args: usageArgs(func(cmd *cobra.Command, args []string) error {
		if len(artifactTag) > 0 {
			if !artifactLatest {
				return usageErrorf("use --latest when resolving by tag")
			}
			if len(args) != 1 {
				return usageErrorf("expected 1 arg (<name-or-path>) when using --tag")
			}
			return nil
		}
		return cobra.ExactArgs(2)(cmd, args)
	}),
	RunE: runArtifactCat,
}

var (
	artifactListSync bool
	artifactOutput   string
	artifactName     string
	artifactTag      []string
	artifactLatest   bool
	artifactAll      bool
	artifactTimeout  time.Duration
	artifactParallel int
	pruneLocalApply  bool
	pruneLocalDryRun bool
	pruneLocalDir    string
	pruneOlderThan   string
	pruneSince       string
	pruneOutputs     bool
	pruneArtifacts   bool
	pruneRemoveEmpty bool
	pruneRecursive   string
)

const defaultArtifactTimeout = 2 * time.Minute

func init() {
	rootCmd.AddCommand(artifactCmd)
	artifactCmd.AddCommand(artifactSyncCmd)
	artifactCmd.AddCommand(artifactListCmd)
	artifactCmd.AddCommand(artifactGetCmd)
	artifactCmd.AddCommand(artifactCatCmd)
	artifactCmd.AddCommand(artifactAddCmd)
	artifactCmd.AddCommand(artifactPruneLocalCmd)

	addArtifactListFlags(artifactListCmd)
	artifactGetCmd.Flags().StringVarP(&artifactOutput, "output", "o", "", "Output path (default: current directory, use '-' for stdout)")
	artifactGetCmd.Flags().BoolVar(&artifactAll, "all", false, "Download all artifacts for the job")
	artifactGetCmd.Flags().DurationVar(&artifactTimeout, "timeout", defaultArtifactTimeout, "Fail if no download progress occurs for this duration")
	artifactGetCmd.Flags().IntVar(&artifactParallel, "parallel", 8, "Maximum concurrent downloads for --all")
	artifactGetCmd.Flags().StringSliceVar(&artifactTag, "tag", nil, "Resolve job ID by tag (can be repeated)")
	artifactGetCmd.Flags().BoolVar(&artifactLatest, "latest", false, "Use the latest job when resolving by tag")
	artifactAddCmd.Flags().StringVar(&artifactName, "name", "", "Optional artifact name")
	artifactCatCmd.Flags().StringSliceVar(&artifactTag, "tag", nil, "Resolve job ID by tag (can be repeated)")
	artifactCatCmd.Flags().BoolVar(&artifactLatest, "latest", false, "Use the latest job when resolving by tag")
	artifactPruneLocalCmd.Flags().BoolVar(&pruneLocalApply, "apply", false, "Delete files (without this flag, only preview)")
	artifactPruneLocalCmd.Flags().BoolVar(&pruneLocalDryRun, "dry-run", false, "Preview deletions without deleting files")
	artifactPruneLocalCmd.Flags().StringVar(&pruneLocalDir, "dir", ".", "Directory scope (default: current directory)")
	artifactPruneLocalCmd.Flags().StringVar(&pruneOlderThan, "older-than", "", "Delete only files older than this duration (e.g. 7d, 48h, or 7)")
	artifactPruneLocalCmd.Flags().StringVar(&pruneSince, "since", "", "Delete only files modified since this time (YYYY-MM-DD, RFC3339, or duration like \"24h ago\")")
	artifactPruneLocalCmd.Flags().BoolVar(&pruneOutputs, "include-outputs", true, "Consider convention output files (output/, outputs/)")
	artifactPruneLocalCmd.Flags().BoolVar(&pruneArtifacts, "include-artifacts", true, "Consider artifact-backed file paths")
	artifactPruneLocalCmd.Flags().BoolVar(&pruneRemoveEmpty, "remove-empty-dirs", false, "Remove empty directories after deleting files")
	artifactPruneLocalCmd.Flags().StringVar(&pruneRecursive, "recursive", "auto", "Recurse into project subdirectories: auto (default; on when --dir isn't itself a project), on, off")
}

func addArtifactListFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&artifactListSync, "sync", false, "Sync artifacts from remote before listing")
}

func runArtifactSync(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return err
	}
	defer database.Close()

	if len(args) == 0 {
		return runArtifactSyncOutstanding(cmd, database)
	}

	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}

	// Build R2 client once for cloud job artifact sync (best-effort, nil if unconfigured)
	var r2Client *r2.Client
	if cfg, err := config.Load(); err == nil {
		r2Client, _ = buildR2Client(cfg)
	}

	var errorsList []string
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: get job: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s not found", ids.FormatJobID(jobID)))
			continue
		}
		if job.IsLaunchJob() {
			result, syncErr := syncCloudJobArtifactsFunc(database, r2Client, job)
			if syncErr != nil {
				if errors.Is(syncErr, artifacts.ErrManifestMissing) {
					// No manifest in R2; try convention-based output sync
					if outResult, outErr := syncJobOutputsFunc(database, job); outErr == nil {
						fmt.Fprintf(cmd.OutOrStdout(), "Job %s: synced %d convention-based outputs\n", ids.FormatJobID(jobID), outResult.Added)
						continue
					}
					errorsList = append(errorsList, fmt.Sprintf("artifact manifest not found for job %s", ids.FormatJobID(jobID)))
					continue
				}
				errorsList = append(errorsList, fmt.Sprintf("job %s: sync cloud artifacts: %v", ids.FormatJobID(jobID), syncErr))
				continue
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Job %s: synced %d artifacts from R2 (skipped %d)\n", ids.FormatJobID(jobID), result.Added, result.Skipped)
			continue
		}
		result, err := artifacts.SyncJob(database, job, NormalSyncTimeout)
		if err != nil {
			if errors.Is(err, artifacts.ErrManifestMissing) {
				// Try convention-based output sync instead
				outResult, syncErr := syncJobOutputsFunc(database, job)
				if syncErr == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "Job %s: synced %d convention-based outputs\n", ids.FormatJobID(jobID), outResult.Added)
					continue
				}
				// Surface the actual fallback failure rather than the misleading
				// "manifest not found": the manifest is expected to be absent for
				// on-prem jobs, so the fallback error is the informative one.
				errorsList = append(errorsList, fmt.Sprintf("job %s: convention-based output sync failed: %v", ids.FormatJobID(jobID), syncErr))
				continue
			}
			if hasFilesystemOutputDeclarations(job) {
				if outResult, syncErr := syncJobOutputsFunc(database, job); syncErr == nil {
					if outResult.Added > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "Job %s: synced %d declared outputs\n", ids.FormatJobID(jobID), outResult.Added)
						continue
					}
				}
			}
			if ssh.IsConnectionError(err.Error()) {
				errorsList = append(errorsList, fmt.Sprintf("host %s unreachable while syncing artifacts for job %s", job.Host, ids.FormatJobID(jobID)))
				continue
			}
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if len(jobIDs) > 1 {
			fmt.Fprintf(cmd.OutOrStdout(), "Job %s: synced %d artifacts (skipped %d)\n", ids.FormatJobID(jobID), result.Added, result.Skipped)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Synced %d artifacts (skipped %d)\n", result.Added, result.Skipped)
		}
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func runArtifactSyncOutstanding(cmd *cobra.Command, database *sql.DB) error {
	jobs, err := db.ListJobs(database, "", "", 0, nil, "")
	if err != nil {
		return err
	}

	// Build R2 client once for cloud job artifact sync
	var r2Client *r2.Client
	if cfg, err := config.Load(); err == nil {
		r2Client, _ = buildR2Client(cfg)
	}

	var totalAdded int
	var totalSkipped int
	var syncedJobs int

	for _, job := range jobs {
		if job.IsLaunchJob() {
			cloudResult, syncErr := syncCloudJobArtifacts(database, r2Client, job)
			if syncErr != nil && !errors.Is(syncErr, artifacts.ErrManifestMissing) {
				fmt.Fprintf(os.Stderr, "Warning: failed to sync cloud artifacts for job %s: %v\n", ids.FormatJobID(job.ID), syncErr)
			}
			if cloudResult.Added > 0 {
				syncedJobs++
				totalAdded += cloudResult.Added
				totalSkipped += cloudResult.Skipped
			}
			continue
		}
		result, err := artifacts.SyncOutstandingJob(database, job, NormalSyncTimeout)
		if err != nil {
			if errors.Is(err, artifacts.ErrManifestMissing) {
				continue
			}
			if ssh.IsConnectionError(err.Error()) {
				fmt.Fprintf(os.Stderr, "Warning: host %s unreachable while syncing artifacts for job %s\n", job.Host, ids.FormatJobID(job.ID))
				continue
			}
			fmt.Fprintf(os.Stderr, "Warning: failed to sync artifacts for job %s: %v\n", ids.FormatJobID(job.ID), err)
			continue
		}
		if result.Added == 0 && result.Skipped == 0 {
			continue
		}
		syncedJobs++
		totalAdded += result.Added
		totalSkipped += result.Skipped
	}

	if totalAdded == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No outstanding artifacts.")
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Synced %d artifacts across %d job(s) (skipped %d)\n", totalAdded, syncedJobs, totalSkipped)
	return nil
}

func runArtifactList(cmd *cobra.Command, args []string) error {
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}
	database, err := db.OpenForReading()
	if err != nil {
		return err
	}
	defer database.Close()

	// Build R2 client once for --sync (best-effort, nil if unconfigured);
	// the resolver uses the same buildArtifactR2Client seam as get/cat.
	var r2Client *r2.Client
	if cfg, err := config.Load(); err == nil {
		r2Client, _ = buildR2Client(cfg)
	}
	store := buildArtifactR2Client()

	var errorsList []string
	for i, jobID := range jobIDs {
		if len(jobIDs) > 1 {
			if i > 0 {
				fmt.Fprintln(cmd.OutOrStdout())
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Job %s:\n", ids.FormatJobID(jobID))
		}

		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: get job: %v", ids.FormatJobID(jobID), err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s not found", ids.FormatJobID(jobID)))
			continue
		}

		// syncedThisRun records whether a host output sync actually completed
		// during this invocation (via --sync or the on-prem auto-sync below).
		// It gates the "no attributable outputs" conclusion: we only claim the
		// run window is empty when discovery actually ran.
		syncedThisRun := false
		if artifactListSync {
			if err := syncArtifactsForJob(database, job, r2Client, NormalSyncTimeout); err != nil {
				if db.IsDatabaseReadOnly(err) || db.IsDatabaseLocked(err) {
					fmt.Fprintf(cmd.ErrOrStderr(), "Warning: skipped artifact sync for job %s; database is not writable, showing cached artifacts\n", ids.FormatJobID(jobID))
				} else {
					errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
					continue
				}
			} else {
				syncedThisRun = true
			}
		}

		resolution, err := resolveJobArtifacts(database, store, job)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), err))
			continue
		}

		if job.IsLaunchJob() {
			warnOnFailedCloudOutputUpload(cmd, job.ID)
		}
		if resolution.r2ListErr != nil {
			// The cloud listing failed: what we print may be missing cloud
			// artifacts. Warn so an incomplete list is never mistaken for the
			// full set.
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: job %s: R2 listing failed; cloud artifacts may be missing from this list: %v\n",
				ids.FormatJobID(jobID), resolution.r2ListErr)
		}

		if len(resolution.rows) == 0 {
			switch {
			case job.EffectiveStatus() == db.StatusQueued && job.StartTime == 0:
				fmt.Fprintln(cmd.OutOrStdout(), "No cached artifacts.")
				fmt.Fprintf(cmd.OutOrStdout(), "Job %s has not started yet; no artifacts are available.\n", ids.FormatJobID(job.ID))
				for _, line := range queuedPlacementLines(database, job) {
					fmt.Fprintf(cmd.OutOrStdout(), "%-12s %s\n", line.Label+":", line.Value)
				}
			case resolution.r2ListErr != nil:
				fmt.Fprintln(cmd.OutOrStdout(), "No cached artifacts found; R2 listing failed, so cloud artifacts could not be checked.")
			case job.HasInventoryHost() && syncedThisRun:
				// Discovery ran and found nothing — state the accurate
				// conclusion instead of the misleading "run sync" hint.
				writeNoAttributableOutputsHint(cmd, job)
			case job.HasInventoryHost():
				fmt.Fprintln(cmd.OutOrStdout(), "No cached artifacts.")
				writeInventoryArtifactHint(cmd, job)
			case job.IsLaunchJob() && !db.IsTerminalStatus(job.Status):
				writeNoCloudArtifactsYetHint(cmd, job)
			default:
				fmt.Fprintln(cmd.OutOrStdout(), "No cached artifacts.")
			}
			continue
		}
		for _, art := range resolution.rows {
			fmt.Fprintln(cmd.OutOrStdout(), art.listLine())
		}
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func warnOnFailedCloudOutputUpload(cmd *cobra.Command, jobID int64) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	logsDir := filepath.Join(home, ".cache", "weft", "logs")

	// Surface manifest_error even when completion.json is absent (e.g. job
	// killed before writing completion).
	manifestErrPath := filepath.Join(logsDir, fmt.Sprintf("%d.manifest_error", jobID))
	if data, err := os.ReadFile(manifestErrPath); err == nil {
		if msg := strings.TrimSpace(string(data)); msg != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: artifact manifest update failed for this job: %s\n", msg)
		}
	}

	completionPath := filepath.Join(logsDir, fmt.Sprintf("%d.completion.json", jobID))
	data, err := os.ReadFile(completionPath)
	if err != nil {
		return
	}
	var rec runner.CompletionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return
	}
	if rec.OutputUpload == nil {
		return
	}
	if rec.OutputUpload.Status != runner.UploadStatusFailed && rec.OutputUpload.Status != runner.UploadStatusPartial {
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: Output upload failed — outputs were not uploaded to R2 (disk may have been full)")
	for _, d := range rec.OutputUpload.Dirs {
		if d.Status != runner.UploadStatusFailed && d.Status != runner.UploadStatusPartial {
			continue
		}
		detail := fmt.Sprintf("  - %s (%d files, %d bytes): %s", d.Dir, d.FileCount, d.Bytes, d.Status)
		if d.Error != "" {
			detail += " — " + d.Error
		}
		fmt.Fprintln(cmd.ErrOrStderr(), detail)
	}
}

func runArtifactGet(cmd *cobra.Command, args []string) error {
	if artifactAll {
		var jobIDs []int64
		if len(artifactTag) > 0 {
			jobID, err := resolveArtifactJobID(args)
			if err != nil {
				return err
			}
			jobIDs = []int64{jobID}
		} else {
			var err error
			jobIDs, err = ParseJobIDs(args)
			if err != nil {
				return err
			}
		}
		return fetchAllArtifactsForJobs(cmd, jobIDs)
	}

	var token string
	if len(artifactTag) > 0 {
		jobID, err := resolveArtifactJobID(args)
		if err != nil {
			return err
		}
		token = args[0]
		return fetchArtifactForJobs(cmd, []int64{jobID}, token)
	} else {
		token = args[len(args)-1]
	}

	jobIDs, err := ParseJobIDs(args[:len(args)-1])
	if err != nil {
		return err
	}
	return fetchArtifactForJobs(cmd, jobIDs, token)
}

func fetchArtifactForJobs(cmd *cobra.Command, jobIDs []int64, token string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return err
	}
	defer database.Close()

	multiple := len(jobIDs) > 1
	var errorsList []string
	for _, jobID := range jobIDs {
		// A nil job (missing row or lookup error) still allows the cache
		// fast path inside deliverArtifactToken.
		job, _ := db.GetJobByID(database, jobID)
		if err := deliverArtifactToken(cmd, database, jobID, job, token, multiple, false); err != nil {
			errorsList = append(errorsList, err.Error())
		}
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

// errR2NotConfigured marks absent R2 config: fall back to host-side sources
// silently; any other R2 failure must be surfaced as a degradation warning
// (spec: Attribution in specs/job-lifecycle.allium).
var errR2NotConfigured = errors.New("R2 not configured")

// artifactSource identifies which store a resolved artifact row came from.
type artifactSource string

const (
	artifactSourceCache artifactSource = "cache"
	artifactSourceHost  artifactSource = "host"
	artifactSourceCloud artifactSource = "cloud"
)

// resolvedArtifact is one row of the shared list/get/cat resolution: the
// union of the local artifact cache, host_data job-output pointer records,
// and the job's live R2 output listing, materialized once per invocation.
// Every row carries the retrieval handle for exactly the bytes the listing
// described, so `get`/`cat` of anything `list` printed cannot resolve
// against a different store than the one that produced the row (the
// list/get mismatch class: bugs wb11, wb16, wb20, wb40).
type resolvedArtifact struct {
	Name        string // manifest artifact name ("" for outputs)
	DisplayPath string // the path `artifact list` prints for this row
	RelPath     string // relative path used for matching and destinations
	SizeBytes   int64
	SHA256      string
	Source      artifactSource

	// Retrieval handles; exactly one is non-nil, matching Source.
	cacheEntry *db.Artifact           // cache: bytes already in the local store
	hostEntry  *dataloc.HostDataEntry // host: pointer record; requires syncArtifactOnDemand
	cloudFile  *runner.OutputFile     // cloud: listed R2 object (R2Key set by the listing)
}

// resolveJobArtifacts materializes the union of the three artifact sources
// for a job — the local cache (`artifacts` table), host_data job-output
// pointer records, and the job's per-run R2 objects — in that order. It is
// the single resolution authority: `artifact list` prints these rows, and
// the `get`/`cat` paths retrieve through the same rows and the same
// matcher (resolvedArtifact.matchesToken).
//
// Pass a nil r2Client to skip the live R2 listing. host_data rows are
// best-effort: the pointer index is auxiliary and its absence (e.g. schema
// not initialized) must not hide cached or cloud artifacts.
// jobArtifactResolution is the outcome of resolveJobArtifacts: the union of
// cache, host-pointer, and cloud rows, plus whether the live R2 listing failed.
// A failed R2 listing is NOT fatal — cached and host rows still serve — but it
// means the cloud set is incomplete, so downstream "not found" messaging must
// read as "status unknown" and `get --all` must not report success (wb40).
type jobArtifactResolution struct {
	rows      []resolvedArtifact
	r2ListErr error // non-nil when the live R2 listing was attempted and (partly) failed
}

func resolveJobArtifacts(database *sql.DB, r2Client cloudOutputStore, job *db.Job) (jobArtifactResolution, error) {
	entries, err := db.ListArtifactsByJob(database, job.ID)
	if err != nil {
		return jobArtifactResolution{}, err
	}
	resolved := cacheResolvedRows(entries)

	outputAssets, _ := listJobOutputAssets(database, job.ID)
	assetPrefix := fmt.Sprintf("%d/", job.ID)
	for i := range outputAssets {
		asset := outputAssets[i]
		resolved = append(resolved, resolvedArtifact{
			DisplayPath: asset.Path,
			RelPath:     strings.TrimPrefix(asset.Asset.ID, assetPrefix),
			SizeBytes:   asset.SizeBytes,
			Source:      artifactSourceHost,
			hostEntry:   &asset,
		})
	}

	// R2 output files exist for any job kind — inventory runners started
	// with --r2-bucket upload to the same per-run prefixes as rentals.
	var r2ListErr error
	if r2Client != nil {
		var files []runner.OutputFile
		files, r2ListErr = listCloudJobOutputFiles(r2Client, job)
		for _, f := range files {
			file := f
			resolved = append(resolved, resolvedArtifact{
				DisplayPath: file.RelPath,
				RelPath:     file.RelPath,
				SizeBytes:   file.SizeBytes,
				Source:      artifactSourceCloud,
				cloudFile:   &file,
			})
		}
	}
	return jobArtifactResolution{rows: resolved, r2ListErr: r2ListErr}, nil
}

// cacheResolvedRows materializes local artifact-cache entries as resolver rows.
func cacheResolvedRows(entries []db.Artifact) []resolvedArtifact {
	rows := make([]resolvedArtifact, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		rows = append(rows, resolvedArtifact{
			Name:        entry.Name,
			DisplayPath: entry.Path,
			RelPath:     entry.Path,
			SizeBytes:   entry.SizeBytes,
			SHA256:      entry.SHA256,
			Source:      artifactSourceCache,
			cacheEntry:  &entry,
		})
	}
	return rows
}

// matchesToken reports whether a user-supplied token names this row. This is
// the one identity matcher shared by list-derived retrieval: it accepts the
// manifest name, the exact printed path, a basename, or a path suffix, with
// the "artifacts/" display prefix canonicalized on both sides — a superset
// of the SQL matching db.FindArtifactByNameOrPath applies to cache rows.
func (a resolvedArtifact) matchesToken(token string) bool {
	if token == "" {
		return false
	}
	if a.Name != "" && a.Name == token {
		return true
	}
	for _, path := range []string{a.RelPath, a.DisplayPath} {
		if path != "" && cloudFileMatchesToken(path, token) {
			return true
		}
	}
	return false
}

// listLine renders the row exactly as `weft artifact list` prints each
// source: cached rows as name/path/size/sha, host pointer rows and cloud
// rows as outputs with their location.
func (a resolvedArtifact) listLine() string {
	switch a.Source {
	case artifactSourceHost:
		return fmt.Sprintf("output\t%s\t%d\t%s", a.DisplayPath, a.SizeBytes, a.hostEntry.Host)
	case artifactSourceCloud:
		return fmt.Sprintf("output\t%s\t%d\tcloud", a.DisplayPath, a.SizeBytes)
	default:
		name := a.Name
		if name == "" {
			name = "-"
		}
		return fmt.Sprintf("%s\t%s\t%d\t%s", name, a.DisplayPath, a.SizeBytes, a.SHA256)
	}
}

// deliverResolvedArtifact writes one resolved row to the destination destFor
// yields ("-" streams to stdout). The row's handle defines retrieval: cache
// rows copy from the local store, cloud rows download the listed R2 object,
// host rows run the on-demand sync and serve the freshly cached entry.
func deliverResolvedArtifact(cmd *cobra.Command, r2Client cloudOutputStore, job *db.Job, art resolvedArtifact, destFor func(string) (string, error)) error {
	switch art.Source {
	case artifactSourceCache:
		return serveCachedArtifact(cmd, art.cacheEntry, destFor)
	case artifactSourceCloud:
		dest, err := destFor(art.cloudFile.RelPath)
		if err != nil {
			return err
		}
		if err := downloadSingleCloudFileToPath(cmd, r2Client, job, *art.cloudFile, dest, dest != "-"); err != nil {
			return err
		}
		if dest != "-" {
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", dest)
		}
		return nil
	case artifactSourceHost:
		synced, err := syncArtifactOnDemand(job, art.RelPath)
		if err != nil {
			return fmt.Errorf("sync from %s failed: %w", job.Host, err)
		}
		if synced == nil {
			return fmt.Errorf("%q: %w", art.RelPath, r2resolve.ErrArtifactMissing)
		}
		return serveCachedArtifact(cmd, synced, destFor)
	}
	return fmt.Errorf("resolved artifact %q has no retrieval handle", art.DisplayPath)
}

// serveCachedArtifact writes a cached artifact row to its destination and
// reports the write on stdout (except when streaming).
func serveCachedArtifact(cmd *cobra.Command, entry *db.Artifact, destFor func(string) (string, error)) error {
	localPath, err := artifacts.LocalPathFromStored(entry.StoredPath)
	if err != nil {
		return err
	}
	dest, err := destFor(localPath)
	if err != nil {
		return err
	}
	if dest == "-" {
		return copyToWriter(localPath, cmd.OutOrStdout())
	}
	if err := copyFile(localPath, dest); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", dest)
	return nil
}

// deliverArtifactToken retrieves the artifact named token for one job and
// writes it to its destination (stream forces stdout, as `cat` does).
//
// Resolution order:
//  1. The local artifact cache (db.FindArtifactByNameOrPath) — serving
//     cached bytes must not depend on R2 reachability. For terminal jobs, a
//     cache row captured before job end yields to a same-path cloud row so
//     final overwrites beat incremental snapshots.
//  2. The shared resolver (resolveJobArtifacts): the same materialized rows
//     `artifact list` prints, matched with the same matcher, each carrying
//     its own retrieval handle — so any spelling list printed resolves here
//     identically.
//  3. An on-demand sync from the job's inventory host, for outputs no
//     listing source has recorded yet.
//
// A row that matches but whose transfer fails surfaces the transfer's real
// cause; "not found" is reported only when every source misses (wb20).
func deliverArtifactToken(cmd *cobra.Command, database *sql.DB, jobID int64, job *db.Job, token string, multiple, stream bool) error {
	destFor := func(source string) (string, error) {
		if stream {
			return "-", nil
		}
		return resolveArtifactOutputPathForJob(source, artifactOutput, jobID, multiple)
	}

	var cachedEntry *db.Artifact
	if entry, err := db.FindArtifactByNameOrPath(database, jobID, token); err == nil {
		localPath, pathErr := artifacts.LocalPathFromStored(entry.StoredPath)
		if pathErr != nil {
			return fmt.Errorf("job %s: %w", ids.FormatJobID(jobID), pathErr)
		}
		if _, statErr := os.Stat(localPath); statErr == nil {
			cachedEntry = entry
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("job %s: %w", ids.FormatJobID(jobID), statErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("job %s: %w", ids.FormatJobID(jobID), err)
	}

	r2Client := buildArtifactR2Client()
	if cachedEntry != nil {
		if cachedArtifactMayPredateFinalUpload(cachedEntry, job) && r2Client != nil {
			resolution, err := resolveJobArtifacts(database, r2Client, job)
			if err != nil {
				return fmt.Errorf("job %s: %w", ids.FormatJobID(job.ID), err)
			}
			if cloudArt, ok := matchingSamePathCloudArtifact(resolution.rows, token, cachedEntry.Path); ok {
				deliverErr := deliverResolvedArtifact(cmd, r2Client, job, cloudArt, destFor)
				if deliverErr == nil {
					return nil
				}
				if !errors.Is(deliverErr, r2resolve.ErrArtifactMissing) && !r2.IsNotFound(deliverErr) {
					return fmt.Errorf("job %s: %w", ids.FormatJobID(job.ID), deliverErr)
				}
			}
		}
		if serveErr := serveCachedArtifact(cmd, cachedEntry, destFor); serveErr != nil {
			return fmt.Errorf("job %s: %w", ids.FormatJobID(jobID), serveErr)
		}
		return nil
	}
	if job == nil {
		return fmt.Errorf("artifact %q not found for job %s", token, ids.FormatJobID(jobID))
	}

	resolution, err := resolveJobArtifacts(database, r2Client, job)
	if err != nil {
		return fmt.Errorf("job %s: %w", ids.FormatJobID(job.ID), err)
	}
	hostSyncRan := false
	for _, art := range resolution.rows {
		if !art.matchesToken(token) {
			continue
		}
		if art.Source == artifactSourceHost {
			hostSyncRan = true
		}
		deliverErr := deliverResolvedArtifact(cmd, r2Client, job, art, destFor)
		if deliverErr == nil {
			return nil
		}
		if art.Source == artifactSourceCache && errors.Is(deliverErr, os.ErrNotExist) {
			continue // stale local cache row; try cloud/host matches
		}
		if errors.Is(deliverErr, r2resolve.ErrArtifactMissing) || r2.IsNotFound(deliverErr) {
			continue // the listing row went stale; try the next match
		}
		return fmt.Errorf("job %s: %w", ids.FormatJobID(job.ID), deliverErr)
	}

	if !hostSyncRan {
		synced, syncErr := syncArtifactOnDemand(job, token)
		if syncErr != nil {
			return fmt.Errorf("artifact %q not cached for job %s, and sync from %s failed: %w",
				token, ids.FormatJobID(job.ID), job.Host, syncErr)
		}
		if synced != nil {
			if serveErr := serveCachedArtifact(cmd, synced, destFor); serveErr != nil {
				return fmt.Errorf("job %s: %w", ids.FormatJobID(job.ID), serveErr)
			}
			return nil
		}
	}
	return errors.New(artifactNotFoundMessage(job, token, r2Client != nil, resolution.r2ListErr != nil))
}

func cachedArtifactMayPredateFinalUpload(entry *db.Artifact, job *db.Job) bool {
	return entry != nil && job != nil && job.EndTime != nil && entry.CreatedAt > 0 && entry.CreatedAt <= *job.EndTime
}

func matchingSamePathCloudArtifact(rows []resolvedArtifact, token, cachedPath string) (resolvedArtifact, bool) {
	cachedCanon := canonicalArtifactRelPath(cachedPath)
	for _, art := range rows {
		if art.Source != artifactSourceCloud || !art.matchesToken(token) {
			continue
		}
		if canonicalArtifactRelPath(art.RelPath) == cachedCanon {
			return art, true
		}
	}
	return resolvedArtifact{}, false
}

// canonicalArtifactRelPath strips the ambiguous "artifacts/" display prefix:
// it normally marks the artifact-files prefix in listings, but a job can also
// write a literal artifacts/ directory under output/. Comparisons between
// listed paths, cached paths, and user tokens should compare this canonical
// form (in addition to the raw spelling where it matters).
func canonicalArtifactRelPath(relPath string) string {
	return strings.TrimPrefix(relPath, "artifacts/")
}

// cloudFileMatchesToken reports whether a listed cloud output file matches a
// user-supplied token: full path, basename, or path suffix, with the
// "artifacts/" display prefix stripped from both sides where applicable.
func cloudFileMatchesToken(relPath, token string) bool {
	canonPath := canonicalArtifactRelPath(relPath)
	canonToken := canonicalArtifactRelPath(token)
	if canonPath == canonToken {
		return true
	}
	if filepath.Base(relPath) == token {
		return true
	}
	return strings.HasSuffix(relPath, "/"+token) || strings.HasSuffix(canonPath, "/"+canonToken)
}

type cloudOutputDownloader interface {
	DownloadObjectToWriterWithIdleTimeout(context.Context, string, io.Writer, time.Duration) (int64, error)
	DownloadObjectToFileWithIdleTimeout(context.Context, string, string, time.Duration) (int64, error)
}

type cloudOutputLister interface {
	ListObjects(context.Context, string) ([]r2.ObjectInfo, error)
}

type cloudOutputStore interface {
	cloudOutputDownloader
	cloudOutputLister
	ObjectExists(context.Context, string) (bool, error)
}

// downloadSingleCloudFileToPath resolves the listed file to a concrete R2 key
// and downloads it once. Resolution failures and transfer failures are
// distinct: a missing object surfaces as r2resolve.ErrArtifactMissing, while
// a stalled or interrupted transfer surfaces with its real cause (it must
// never be reported as "not found" — see wb20).
func downloadSingleCloudFileToPath(cmd *cobra.Command, r2Client cloudOutputStore, job *db.Job, f runner.OutputFile, dest string, showProgress bool) error {
	key := f.R2Key
	if key == "" {
		var err error
		key, err = resolveCloudOutputKey(context.Background(), r2Client, job, f.RelPath)
		if err != nil {
			return err
		}
	}
	if dest == "-" {
		if _, err := r2Client.DownloadObjectToWriterWithIdleTimeout(context.Background(), key, cmd.OutOrStdout(), artifactTimeout); err != nil {
			return fmt.Errorf("download %s from R2: %w", key, err)
		}
		return nil
	}
	var status *statusLine
	if showProgress && f.SizeBytes >= downloadProgressThresholdBytes {
		status = newStatusLine(cmd.ErrOrStderr())
	}
	if err := downloadCloudKeyToPathAtomic(r2Client, key, dest, artifactTimeout, status, f.RelPath, f.SizeBytes); err != nil {
		return fmt.Errorf("download %s from R2: %w", key, err)
	}
	return nil
}

// resolveCloudOutputKey maps a listed/display rel path to a concrete R2 key
// via the shared resolution policy (latest run, run zero, then runs
// discovered in R2 — the superseded-attempt case). The "artifacts/" display
// prefix is ambiguous: it normally marks the artifact-files prefix, but a
// job can also write a literal artifacts/ directory under output/. The
// trimmed spelling is probed first (the common case), then the raw one.
func resolveCloudOutputKey(ctx context.Context, store cloudOutputStore, job *db.Job, relPath string) (string, error) {
	rels := []string{relPath}
	if trimmed := canonicalArtifactRelPath(relPath); trimmed != relPath {
		rels = []string{trimmed, relPath}
	}
	var lastMissing error
	for _, rel := range rels {
		key, err := r2resolve.NeedR2Key(ctx, store, job.ID, job.LatestRunID, rel)
		if err == nil {
			return key, nil
		}
		if !errors.Is(err, r2resolve.ErrArtifactMissing) {
			return "", err
		}
		lastMissing = err
	}
	return "", lastMissing
}

// downloadCloudOutputFiles downloads all cloud output files for a job.
func downloadCloudOutputFiles(cmd *cobra.Command, r2Client cloudOutputStore, job *db.Job, files []runner.OutputFile, multiple bool) (int, error) {
	if len(files) == 0 {
		return 0, nil
	}
	parallel := artifactParallel
	if parallel <= 0 {
		parallel = 1
	}
	if parallel > len(files) {
		parallel = len(files)
	}

	type result struct {
		file runner.OutputFile
		dest string
		err  error
	}
	jobs := make(chan runner.OutputFile)
	results := make(chan result, len(files))
	for i := 0; i < parallel; i++ {
		go func() {
			for f := range jobs {
				dest, err := resolveArtifactOutputPathForAll(f.RelPath, artifactOutput, job.ID, multiple)
				if err == nil {
					err = downloadSingleCloudFileToPath(cmd, r2Client, job, f, dest, false)
				}
				results <- result{file: f, dest: dest, err: err}
			}
		}()
	}
	go func() {
		for _, f := range files {
			jobs <- f
		}
		close(jobs)
	}()

	var errorsList []string
	var downloaded int
	for range files {
		res := <-results
		if res.err != nil {
			errorsList = append(errorsList, fmt.Sprintf("download %s: %v", res.file.RelPath, res.err))
			continue
		}
		downloaded++
		if res.dest != "-" {
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", res.dest)
		}
	}
	if len(errorsList) > 0 {
		return downloaded, errors.New(strings.Join(errorsList, "; "))
	}
	return downloaded, nil
}

// fetchAllArtifactsForJobs delivers every artifact `weft artifact list`
// would print for each job, through the same resolver (contract:
// ArtifactResolution / rule ArtifactListGetConsistency in
// specs/job-lifecycle.allium), deduplicated per dedupeResolvedForAll.
func fetchAllArtifactsForJobs(cmd *cobra.Command, jobIDs []int64) error {
	database, err := db.OpenForReading()
	if err != nil {
		return err
	}
	defer database.Close()

	r2Client := buildArtifactR2Client()

	var errorsList []string
	for _, jobID := range jobIDs {
		job, jobErr := db.GetJobByID(database, jobID)
		if jobErr != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: get job: %v", ids.FormatJobID(jobID), jobErr))
			continue
		}
		resolution, resolveErr := resolveAllArtifactRows(database, r2Client, jobID, job)
		if resolveErr != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), resolveErr))
			continue
		}

		if job != nil && job.HasInventoryHost() && rowsNeedHostSync(resolution.rows) {
			// Outputs may exist only on the job's host: sync at most once per
			// job, then re-resolve so host-pointer rows become cache rows.
			if _, syncErr := syncArtifactOnDemand(job, ""); syncErr != nil {
				errorsList = append(errorsList, fmt.Sprintf("job %s: sync from %s: %v", ids.FormatJobID(jobID), job.Host, syncErr))
				continue
			}
			if resolution, resolveErr = resolveAllArtifactRows(database, r2Client, jobID, job); resolveErr != nil {
				errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), resolveErr))
				continue
			}
		}
		rows, r2ListErr := resolution.rows, resolution.r2ListErr
		if r2ListErr != nil {
			// R2 listing failed: the cloud file set is incomplete, so `get --all`
			// must not silently omit R2-only files and exit 0. Record the error
			// (which forces a non-zero exit) but still deliver what did resolve.
			errorsList = append(errorsList, fmt.Sprintf("job %s: R2 listing failed, some cloud artifacts may be missing: %v", ids.FormatJobID(jobID), r2ListErr))
		}

		if len(rows) == 0 {
			if r2ListErr != nil {
				// Already recorded in errorsList; don't claim a confident "none".
				fmt.Fprintf(cmd.OutOrStdout(), "Job %s: R2 listing failed; artifact availability could not be confirmed\n", ids.FormatJobID(jobID))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Job %s: no artifacts found\n", ids.FormatJobID(jobID))
			}
			continue
		}

		multiple := len(rows) > 1 || len(jobIDs) > 1
		var cloudFiles []runner.OutputFile
		for _, art := range rows {
			if art.Source == artifactSourceCloud {
				cloudFiles = append(cloudFiles, *art.cloudFile)
				continue
			}
			dest, destErr := resolveArtifactOutputPathForAll(art.DisplayPath, artifactOutput, jobID, multiple)
			if destErr != nil {
				return destErr
			}
			destFor := func(string) (string, error) { return dest, nil }
			if deliverErr := deliverResolvedArtifact(cmd, r2Client, job, art, destFor); deliverErr != nil {
				errorsList = append(errorsList, fmt.Sprintf("job %s artifact %q: %v", ids.FormatJobID(jobID), art.DisplayPath, deliverErr))
			}
		}
		if len(cloudFiles) > 0 {
			if _, dlErr := downloadCloudOutputFiles(cmd, r2Client, job, cloudFiles, multiple); dlErr != nil {
				errorsList = append(errorsList, fmt.Sprintf("job %s: %v", ids.FormatJobID(jobID), dlErr))
			}
		}
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

// rowsNeedHostSync reports whether `get --all` should run the one-per-job
// host output sync before delivering: nothing resolved at all, or some rows
// are still host-pointer records without cached bytes.
func rowsNeedHostSync(rows []resolvedArtifact) bool {
	if len(rows) == 0 {
		return true
	}
	for _, row := range rows {
		if row.Source == artifactSourceHost {
			return true
		}
	}
	return false
}

// resolveAllArtifactRows materializes the deduplicated row set `get --all`
// delivers for one job. A nil job row still serves the local cache.
func resolveAllArtifactRows(database *sql.DB, r2Client cloudOutputStore, jobID int64, job *db.Job) (jobArtifactResolution, error) {
	if job == nil {
		entries, err := db.ListArtifactsByJob(database, jobID)
		if err != nil {
			return jobArtifactResolution{}, err
		}
		return jobArtifactResolution{rows: cacheResolvedRows(entries)}, nil
	}
	resolution, err := resolveJobArtifacts(database, r2Client, job)
	if err != nil {
		return jobArtifactResolution{}, err
	}
	resolution.rows = dedupeResolvedForAll(job, resolution.rows)
	return resolution, nil
}

// dedupeResolvedForAll keeps one row per canonical path, preferring the
// local cache, then the cloud listing, then host-pointer records. Two
// exceptions, per rule ArtifactListGetConsistency
// (specs/job-lifecycle.allium): a terminal job's pre-end cache row yields
// to its same-path cloud row (wb57), and same-source rows whose sizes
// disagree are distinct objects and are all kept.
func dedupeResolvedForAll(job *db.Job, rows []resolvedArtifact) []resolvedArtifact {
	rank := func(row resolvedArtifact) int {
		switch row.Source {
		case artifactSourceCache:
			if cachedArtifactMayPredateFinalUpload(row.cacheEntry, job) {
				return 2
			}
			return 0
		case artifactSourceCloud:
			return 1
		default:
			return 3
		}
	}
	bestIdx := make(map[string]int, len(rows))
	out := make([]resolvedArtifact, 0, len(rows))
	for _, row := range rows {
		key := canonicalArtifactRelPath(row.RelPath)
		i, ok := bestIdx[key]
		if !ok {
			bestIdx[key] = len(out)
			out = append(out, row)
			continue
		}
		if row.Source == out[i].Source && row.SizeBytes != out[i].SizeBytes {
			out = append(out, row)
			continue
		}
		if rank(row) < rank(out[i]) {
			out[i] = row
		}
	}
	return out
}

func runArtifactCat(cmd *cobra.Command, args []string) error {
	var token string
	jobID, err := resolveArtifactJobID(args)
	if err != nil {
		return err
	}
	if len(artifactTag) > 0 {
		token = args[0]
	} else {
		token = args[1]
	}

	database, err := db.OpenForReading()
	if err != nil {
		return err
	}
	defer database.Close()

	// A nil job (missing row or lookup error) still allows the cache fast
	// path inside deliverArtifactToken.
	job, _ := db.GetJobByID(database, jobID)
	return deliverArtifactToken(cmd, database, jobID, job, token, false, true)
}

func runArtifactAdd(cmd *cobra.Command, args []string) error {
	jobID, err := parseJobID(args[0])
	if err != nil {
		return err
	}
	artifactPath := strings.TrimSpace(args[1])
	if artifactPath == "" {
		return usageErrorf("artifact path is required")
	}

	database, err := db.Open()
	if err != nil {
		return err
	}
	defer database.Close()

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}

	if err := ensureRemoteArtifactDir(job.Host); err != nil {
		return err
	}

	manifest, err := fetchOrInitManifest(job.Host, jobID)
	if err != nil {
		return err
	}

	manifest.Artifacts = upsertArtifactSpec(manifest.Artifacts, artifacts.ArtifactSpec{
		Name: strings.TrimSpace(artifactName),
		Path: artifactPath,
	})

	if err := writeRemoteManifest(job.Host, manifest); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Added artifact to %s\n", artifacts.RemoteManifestPath(jobID))
	return nil
}

func parseJobID(raw string) (int64, error) {
	id, err := ids.ParseJobID(raw)
	if err != nil || id <= 0 {
		return 0, usageErrorf("invalid job id %q", raw)
	}
	return id, nil
}

func resolveArtifactJobID(args []string) (int64, error) {
	if len(artifactTag) == 0 {
		return parseJobID(args[0])
	}

	database, err := db.OpenForReading()
	if err != nil {
		return 0, err
	}
	defer database.Close()

	jobs, err := db.ListJobsWithMaxAge(database, "", "", 0, 0, artifactTag, "")
	if err != nil {
		return 0, err
	}
	if len(jobs) == 0 {
		return 0, fmt.Errorf("no jobs found with tags: %s", strings.Join(artifactTag, ", "))
	}
	if !artifactLatest {
		return 0, usageErrorf("use --latest when resolving by tag")
	}
	var latestID int64
	for _, job := range jobs {
		if job.ID > latestID {
			latestID = job.ID
		}
	}
	if latestID == 0 {
		return 0, fmt.Errorf("no jobs found with tags: %s", strings.Join(artifactTag, ", "))
	}
	return latestID, nil
}

func resolveArtifactOutputPath(source, output string) (string, error) {
	if output == "" {
		return filepath.Base(source), nil
	}
	if output == "-" {
		return "-", nil
	}
	info, err := os.Stat(output)
	if err == nil && info.IsDir() {
		return filepath.Join(output, filepath.Base(source)), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return output, nil
}

func resolveArtifactOutputPathForJob(source, output string, jobID int64, multiple bool) (string, error) {
	if !multiple {
		return resolveArtifactOutputPath(source, output)
	}
	if output == "-" {
		return "", fmt.Errorf("cannot use stdout when retrieving multiple artifacts")
	}
	if output == "" {
		return fmt.Sprintf("%d-%s", jobID, filepath.Base(source)), nil
	}
	info, err := os.Stat(output)
	if err == nil && info.IsDir() {
		return filepath.Join(output, fmt.Sprintf("%d-%s", jobID, filepath.Base(source))), nil
	}
	if err == nil {
		return "", fmt.Errorf("output must be a directory when retrieving multiple artifacts")
	}
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("output must be an existing directory when retrieving multiple artifacts")
	}
	return "", err
}

func resolveArtifactOutputPathForAll(source, output string, jobID int64, multiple bool) (string, error) {
	rel, ok := normalizeLocalRelPath(source)
	if !ok {
		return "", fmt.Errorf("invalid artifact path %q", source)
	}
	if output == "-" {
		if multiple {
			return "", fmt.Errorf("cannot use stdout when retrieving multiple artifacts")
		}
		return "-", nil
	}
	base := "."
	if strings.TrimSpace(output) != "" {
		info, err := os.Stat(output)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("output must be an existing directory when retrieving all artifacts")
			}
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("output must be a directory when retrieving all artifacts")
		}
		base = output
	}
	if multiple {
		base = filepath.Join(base, ids.FormatJobID(jobID))
	}
	return filepath.Join(base, filepath.FromSlash(rel)), nil
}

// downloadProgressThresholdBytes is the payload size above which single-file
// downloads show a progress status line (TTY-only). Below it the transfer
// finishes faster than the line would be useful.
const downloadProgressThresholdBytes = 8 << 20

// progressStatusWriter counts bytes through to dst and reports transfer
// progress on a status line at most twice per second.
type progressStatusWriter struct {
	dst    io.Writer
	status *statusLine
	label  string
	total  int64
	done   int64
	lastAt time.Time
}

func (w *progressStatusWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.done += int64(n)
	if now := time.Now(); now.Sub(w.lastAt) >= 500*time.Millisecond {
		w.lastAt = now
		if w.total > 0 {
			w.status.Update(fmt.Sprintf("%s: %s / %s", w.label, humanizeBytes(w.done), humanizeBytes(w.total)))
		} else {
			w.status.Update(fmt.Sprintf("%s: %s", w.label, humanizeBytes(w.done)))
		}
	}
	return n, err
}

// downloadCloudKeyToPathAtomic streams an R2 object to dest via a .part
// sibling, optionally reporting progress on status.
func downloadCloudKeyToPathAtomic(r2Client cloudOutputDownloader, key, dest string, idleTimeout time.Duration, status *statusLine, label string, totalBytes int64) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmpDest := dest + ".part"
	f, err := os.Create(tmpDest)
	if err != nil {
		return err
	}
	var w io.Writer = f
	if status != nil {
		w = &progressStatusWriter{dst: f, status: status, label: label, total: totalBytes}
	}
	_, dlErr := r2Client.DownloadObjectToWriterWithIdleTimeout(context.Background(), key, w, idleTimeout)
	closeErr := f.Close()
	if status != nil {
		status.Clear()
	}
	if dlErr != nil {
		_ = os.Remove(tmpDest)
		return dlErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpDest)
		return closeErr
	}
	return os.Rename(tmpDest, dest)
}

func copyFile(src, dest string) error {
	tmpDest := dest + ".part"
	if err := copyFileDirect(src, tmpDest); err != nil {
		_ = os.Remove(tmpDest)
		return err
	}
	return os.Rename(tmpDest, dest)
}

func copyFileDirect(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func copyToWriter(src string, out io.Writer) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	_, err = io.Copy(out, in)
	return err
}

func ensureRemoteArtifactDir(host string) error {
	cmd := fmt.Sprintf("mkdir -p %s", artifacts.RemoteArtifactsDir)
	_, _, err := ssh.RunWithTimeout(host, cmd, NormalSyncTimeout)
	return err
}

func fetchOrInitManifest(host string, jobID int64) (artifacts.Manifest, error) {
	manifest, err := artifacts.FetchManifest(host, jobID, NormalSyncTimeout)
	if err != nil {
		if errors.Is(err, artifacts.ErrManifestMissing) {
			return artifacts.Manifest{JobID: jobID}, nil
		}
		return artifacts.Manifest{}, err
	}
	return manifest, nil
}

func writeRemoteManifest(host string, manifest artifacts.Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	content := string(append(data, '\n'))
	manifestPath := artifacts.RemoteManifestPath(manifest.JobID)
	cmd := fmt.Sprintf("cat > %s << 'ARTIFACT_EOF'\n%s\nARTIFACT_EOF", manifestPath, content)
	_, _, err = ssh.RunWithTimeout(host, cmd, NormalSyncTimeout)
	return err
}

func upsertArtifactSpec(specs []artifacts.ArtifactSpec, spec artifacts.ArtifactSpec) []artifacts.ArtifactSpec {
	if spec.Path == "" {
		return specs
	}
	for i, existing := range specs {
		if spec.Name != "" && existing.Name == spec.Name {
			specs[i] = spec
			return specs
		}
		if existing.Path == spec.Path {
			specs[i] = spec
			return specs
		}
	}
	return append(specs, spec)
}

type pruneTimeFilter struct {
	olderThan time.Duration
	since     time.Time
}

type localPruneCandidate struct {
	AbsPath string
	RelPath string
	Size    int64
	ModTime time.Time
}

type localPrunePlan struct {
	ScopeRoot              string
	ScannedCandidateFiles  int
	RestorableMatchedFiles int
	TimeFilteredFiles      int
	Files                  []localPruneCandidate
	Bytes                  int64
	Warnings               []string
}

func runArtifactPruneLocal(cmd *cobra.Command, _ []string) error {
	if strings.TrimSpace(pruneOlderThan) != "" && strings.TrimSpace(pruneSince) != "" {
		return usageErrorf("--older-than and --since are mutually exclusive")
	}
	if !pruneOutputs && !pruneArtifacts {
		return usageErrorf("nothing selected: enable --include-outputs and/or --include-artifacts")
	}

	scopeRoot, err := resolveLocalPruneScope(pruneLocalDir)
	if err != nil {
		return err
	}

	filter, err := parsePruneTimeFilter(pruneOlderThan, pruneSince, time.Now())
	if err != nil {
		return err
	}

	database, err := db.OpenForReading()
	if err != nil {
		return err
	}
	defer database.Close()

	progress := newStatusLine(cmd.ErrOrStderr())
	defer progress.Clear()

	progress.Update("Loading jobs...")
	jobs, err := db.ListJobsByStatuses(database, nil, "", "", 0, nil, "")
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	scopes, err := resolvePruneScopes(jobs, scopeRoot, pruneRecursive)
	if err != nil {
		return err
	}

	effectiveDryRun := !pruneLocalApply || pruneLocalDryRun
	multiProject := len(scopes) > 1
	totalDeleteFailures := 0
	var rollupFiles int
	var rollupBytes int64
	for i, scope := range scopes {
		if multiProject {
			if i > 0 {
				fmt.Fprintln(cmd.OutOrStdout())
			}
			rel, _ := filepath.Rel(scopeRoot, scope)
			if rel == "" || rel == "." {
				rel = scope
			}
			fmt.Fprintf(cmd.OutOrStdout(), "== %s ==\n", rel)
		}
		result, err := pruneOneScope(cmd, database, scope, filter, effectiveDryRun, progress, jobs)
		if err != nil {
			return err
		}
		totalDeleteFailures += result.DeleteFailures
		rollupFiles += result.Files
		rollupBytes += result.Bytes
	}
	if multiProject {
		printPruneRollup(cmd, len(scopes), rollupFiles, rollupBytes, effectiveDryRun)
	}
	if totalDeleteFailures > 0 {
		return fmt.Errorf("failed to delete %d file(s)", totalDeleteFailures)
	}
	return nil
}

// printPruneRollup writes the cross-project total after per-scope output when
// prune-local ran over more than one project.
func printPruneRollup(cmd *cobra.Command, projects, files int, bytes int64, dryRun bool) {
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintf(cmd.OutOrStdout(), "== Total (%d projects) ==\n", projects)
	if dryRun {
		fmt.Fprintf(
			cmd.OutOrStdout(),
			"Would free: %s (%s bytes) from %d file(s) across %d projects\n",
			humanizeBytes(bytes), formatIntWithCommas(bytes), files, projects,
		)
	} else {
		fmt.Fprintf(
			cmd.OutOrStdout(),
			"Deleted: %d file(s), reclaimed %s (%s bytes) across %d projects\n",
			files, humanizeBytes(bytes), formatIntWithCommas(bytes), projects,
		)
	}
}

func pruneOneScope(cmd *cobra.Command, database *sql.DB, scopeRoot string, filter pruneTimeFilter, effectiveDryRun bool, progress *statusLine, jobs []*db.Job) (pruneScopeResult, error) {
	plan, err := buildLocalPrunePlan(database, scopeRoot, pruneOutputs, pruneArtifacts, filter, progress, jobs)
	if err != nil {
		return pruneScopeResult{}, err
	}
	progress.Clear()

	for _, warning := range plan.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", warning)
	}

	if len(plan.Files) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No restorable local files matched in %s\n", plan.ScopeRoot)
		printLocalPruneSummary(cmd, plan, effectiveDryRun, 0, 0)
		return pruneScopeResult{}, nil
	}

	sort.Slice(plan.Files, func(i, j int) bool {
		return plan.Files[i].RelPath < plan.Files[j].RelPath
	})
	for _, f := range plan.Files {
		verb := "Would delete"
		if !effectiveDryRun {
			verb = "Deleting"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s (%s)\n", verb, f.RelPath, humanizeBytes(f.Size))
	}

	deleteFailures := 0
	deletedFiles := 0
	var deletedBytes int64
	if !effectiveDryRun {
		for _, f := range plan.Files {
			if err := os.Remove(f.AbsPath); err != nil {
				deleteFailures++
				fmt.Fprintf(cmd.ErrOrStderr(), "Failed to delete %s: %v\n", f.RelPath, err)
				continue
			}
			deletedFiles++
			deletedBytes += f.Size
		}
		if pruneRemoveEmpty {
			removed, err := removeEmptyOutputDirs(scopeRoot)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: remove empty dirs: %v\n", err)
			} else if removed > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Removed %d empty directories.\n", removed)
			}
		}
	}

	if effectiveDryRun {
		fmt.Fprintf(cmd.OutOrStdout(), "Would free: %s (%d bytes)\n", humanizeBytes(plan.Bytes), plan.Bytes)
		printLocalPruneSummary(cmd, plan, true, len(plan.Files), plan.Bytes)
		fmt.Fprintf(cmd.OutOrStdout(), "Dry run: no files deleted.\n")
		fmt.Fprintf(cmd.OutOrStdout(), "To apply these deletions, run: %s\n", buildPruneLocalApplyCommand())
		return pruneScopeResult{Files: len(plan.Files), Bytes: plan.Bytes}, nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Deleted: %d file(s), reclaimed %s (%d bytes)\n", deletedFiles, humanizeBytes(deletedBytes), deletedBytes)
	printLocalPruneSummary(cmd, plan, false, deletedFiles, deletedBytes)
	return pruneScopeResult{Files: deletedFiles, Bytes: deletedBytes, DeleteFailures: deleteFailures}, nil
}

// pruneScopeResult captures the per-scope outcome so the caller can accumulate
// a cross-project rollup. Files/Bytes are the effective totals: would-free in
// dry-run mode, actually-deleted in apply mode.
type pruneScopeResult struct {
	Files          int
	Bytes          int64
	DeleteFailures int
}

// resolvePruneScopes returns the set of scope roots to prune. When recursive
// mode is "auto" (default), it inspects the job list: if scopeRoot itself has
// no associated jobs but its child directories do, the child project dirs are
// returned instead. "on" forces child-only mode; "off" forces single-scope.
func resolvePruneScopes(jobs []*db.Job, scopeRoot, mode string) ([]string, error) {
	mode = strings.TrimSpace(strings.ToLower(mode))
	switch mode {
	case "", "auto", "on", "off":
	default:
		return nil, usageErrorf("--recursive must be one of: auto, on, off")
	}
	if mode == "off" {
		return []string{scopeRoot}, nil
	}
	subdirs, scopeIsProject := findProjectSubdirs(jobs, scopeRoot)
	if mode == "on" {
		if len(subdirs) == 0 {
			return []string{scopeRoot}, nil
		}
		return subdirs, nil
	}
	if scopeIsProject || len(subdirs) == 0 {
		return []string{scopeRoot}, nil
	}
	return subdirs, nil
}

// findProjectSubdirs returns the immediate child directories of scopeRoot that
// contain weft jobs (i.e. have been used as a job's working_dir, or contain a
// directory that has). scopeIsProject is true if scopeRoot itself is a job
// working_dir.
func findProjectSubdirs(jobs []*db.Job, scopeRoot string) ([]string, bool) {
	scopeIsProject := false
	seen := map[string]struct{}{}
	for _, job := range jobs {
		local := workdir.ResolveLocal(job.EffectiveWorkingDir())
		if local == "" {
			continue
		}
		rel, err := filepath.Rel(scopeRoot, local)
		if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
			continue
		}
		if rel == "." {
			scopeIsProject = true
			continue
		}
		first := strings.SplitN(rel, string(filepath.Separator), 2)[0]
		if first == "" || first == "." {
			continue
		}
		seen[filepath.Join(scopeRoot, first)] = struct{}{}
	}
	subdirs := make([]string, 0, len(seen))
	for d := range seen {
		subdirs = append(subdirs, d)
	}
	sort.Strings(subdirs)
	return subdirs, scopeIsProject
}

// statusLine writes a single overwriting status line to the given writer when
// it is a TTY. It is safe to call all methods on a nil receiver and on a
// non-TTY writer; both no-op.
type statusLine struct {
	w       io.Writer
	enabled bool
	lastLen int
}

func newStatusLine(w io.Writer) *statusLine {
	s := &statusLine{w: w}
	if f, ok := w.(*os.File); ok {
		s.enabled = term.IsTerminal(f.Fd())
	}
	return s
}

func (s *statusLine) Update(msg string) {
	if s == nil || !s.enabled {
		return
	}
	pad := ""
	if len(msg) < s.lastLen {
		pad = strings.Repeat(" ", s.lastLen-len(msg))
	}
	fmt.Fprintf(s.w, "\r%s%s", msg, pad)
	s.lastLen = len(msg)
}

func (s *statusLine) Clear() {
	if s == nil || !s.enabled || s.lastLen == 0 {
		return
	}
	fmt.Fprintf(s.w, "\r%s\r", strings.Repeat(" ", s.lastLen))
	s.lastLen = 0
}

func printLocalPruneSummary(cmd *cobra.Command, plan localPrunePlan, dryRun bool, effectiveFiles int, effectiveBytes int64) {
	mode := "dry-run"
	if !dryRun {
		mode = "apply"
	}
	bytesLabel := "would_free"
	if dryRun {
		bytesLabel = "would_free"
	} else {
		bytesLabel = "deleted"
	}
	bytesWithCommas := formatIntWithCommas(effectiveBytes)
	fmt.Fprintf(
		cmd.OutOrStdout(),
		"Summary (%s): scanned %d, restorable %d, time-filtered %d, selected %d, %s %s bytes (%s)\n",
		mode,
		plan.ScannedCandidateFiles,
		plan.RestorableMatchedFiles,
		plan.TimeFilteredFiles,
		effectiveFiles,
		bytesLabel,
		bytesWithCommas,
		humanizeBytes(effectiveBytes),
	)
}

func formatIntWithCommas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	negative := false
	if s[0] == '-' {
		negative = true
		s = s[1:]
	}
	rem := len(s) % 3
	var b strings.Builder
	if negative {
		b.WriteByte('-')
	}
	if rem > 0 {
		b.WriteString(s[:rem])
		if len(s) > rem {
			b.WriteByte(',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

func buildPruneLocalApplyCommand() string {
	args := []string{"weft", "artifact", "prune-local", "--apply"}
	if strings.TrimSpace(pruneLocalDir) != "" && strings.TrimSpace(pruneLocalDir) != "." {
		args = append(args, "--dir", shellQuote(pruneLocalDir))
	}
	if strings.TrimSpace(pruneOlderThan) != "" {
		args = append(args, "--older-than", shellQuote(pruneOlderThan))
	}
	if strings.TrimSpace(pruneSince) != "" {
		args = append(args, "--since", shellQuote(pruneSince))
	}
	if !pruneOutputs {
		args = append(args, "--include-outputs=false")
	}
	if !pruneArtifacts {
		args = append(args, "--include-artifacts=false")
	}
	if pruneRemoveEmpty {
		args = append(args, "--remove-empty-dirs")
	}
	return strings.Join(args, " ")
}

func resolveLocalPruneScope(dir string) (string, error) {
	cleaned := strings.TrimSpace(dir)
	if cleaned == "" {
		cleaned = "."
	}
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", fmt.Errorf("resolve --dir %q: %w", dir, err)
	}
	return filepath.Clean(abs), nil
}

func parsePruneTimeFilter(olderThanRaw, sinceRaw string, now time.Time) (pruneTimeFilter, error) {
	var filter pruneTimeFilter
	if strings.TrimSpace(olderThanRaw) != "" && strings.TrimSpace(sinceRaw) != "" {
		return filter, usageErrorf("--older-than and --since are mutually exclusive")
	}
	if strings.TrimSpace(olderThanRaw) != "" {
		d, err := parseOlderThanDuration(olderThanRaw)
		if err != nil {
			return filter, err
		}
		filter.olderThan = d
	}
	if strings.TrimSpace(sinceRaw) != "" {
		cutoff, err := parseSinceCutoff(sinceRaw, now)
		if err != nil {
			return filter, err
		}
		filter.since = cutoff
	}
	return filter, nil
}

func parseOlderThanDuration(raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, usageErrorf("--older-than requires a value")
	}
	if days, err := strconv.Atoi(trimmed); err == nil {
		if days <= 0 {
			return 0, usageErrorf("invalid --older-than %q (must be > 0)", raw)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := parseDuration(trimmed)
	if err != nil {
		return 0, usageErrorf("invalid --older-than %q (use duration like 7d, 48h, or integer days)", raw)
	}
	if d <= 0 {
		return 0, usageErrorf("invalid --older-than %q (must be > 0)", raw)
	}
	return d, nil
}

func buildLocalPrunePlan(database *sql.DB, scopeRoot string, includeOutputs bool, includeArtifacts bool, filter pruneTimeFilter, progress *statusLine, jobs []*db.Job) (localPrunePlan, error) {
	plan := localPrunePlan{ScopeRoot: scopeRoot}

	project, _ := workdir.ResolveProjectName("", scopeRoot)
	if jobs == nil {
		var err error
		jobs, err = db.ListJobsByStatuses(database, nil, "", "", 0, nil, "")
		if err != nil {
			return plan, fmt.Errorf("list jobs: %w", err)
		}
	}
	scopeJobs := filterJobsForScope(jobs, scopeRoot, project)

	restorable := make(map[string]struct{})
	if includeOutputs {
		cfg, cfgErr := config.Load()
		var r2Client *r2.Client
		if cfgErr == nil {
			r2Client, _ = buildR2Client(cfg)
		}
		if r2Client == nil {
			plan.Warnings = append(plan.Warnings, "R2 not configured; cloud output discovery is limited to locally indexed entries")
		}
		for i, job := range scopeJobs {
			progress.Update(fmt.Sprintf("Scanning R2 outputs for %s (%d/%d)...", filepath.Base(scopeRoot), i+1, len(scopeJobs)))
			entries, err := listJobOutputAssets(database, job.ID)
			if err == nil {
				for _, entry := range entries {
					if rel, ok := normalizeLocalRelPath(entry.Path); ok {
						restorable[rel] = struct{}{}
					}
				}
			}
			if r2Client != nil && job.IsLaunchJob() {
				// A list error is intentionally ignored here: this builds the
				// set of files considered restorable from R2 before a local
				// prune. On error the set only shrinks, so fewer local files are
				// treated as safe to delete — the conservative, non-destructive
				// direction. Never expand `restorable` on unknown R2 status.
				files, _ := listCloudJobOutputFiles(r2Client, job)
				for _, f := range files {
					if rel, ok := normalizeLocalRelPath(f.RelPath); ok {
						restorable[rel] = struct{}{}
					}
				}
			}
		}
	}
	if includeArtifacts {
		for i, job := range scopeJobs {
			progress.Update(fmt.Sprintf("Scanning artifacts for %s (%d/%d)...", filepath.Base(scopeRoot), i+1, len(scopeJobs)))
			arts, err := db.ListArtifactsByJob(database, job.ID)
			if err != nil {
				continue
			}
			for _, art := range arts {
				if rel, ok := normalizeLocalRelPath(art.Path); ok {
					restorable[rel] = struct{}{}
				}
			}
		}
	}

	progress.Update(fmt.Sprintf("Walking %s...", filepath.Base(scopeRoot)))
	candidates, err := collectLocalPruneCandidates(scopeRoot, includeArtifacts, restorable)
	if err != nil {
		return plan, err
	}
	plan.ScannedCandidateFiles = len(candidates)

	now := time.Now()
	for _, candidate := range candidates {
		if _, ok := restorable[candidate.RelPath]; !ok {
			continue
		}
		plan.RestorableMatchedFiles++
		if !passesPruneTimeFilter(candidate.ModTime, filter, now) {
			plan.TimeFilteredFiles++
			continue
		}
		plan.Files = append(plan.Files, candidate)
		plan.Bytes += candidate.Size
	}
	return plan, nil
}

func filterJobsForScope(jobs []*db.Job, scopeRoot string, project string) []*db.Job {
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
		if localDir != "" && isWithinScope(scopeRoot, localDir) {
			filtered = append(filtered, job)
			continue
		}
		if project != "" && strings.TrimSpace(job.Project) == project {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func collectLocalPruneCandidates(scopeRoot string, includeArtifacts bool, restorable map[string]struct{}) ([]localPruneCandidate, error) {
	outputDirs := config.ProjectOutputDirs(scopeRoot)
	seen := make(map[string]struct{})
	result := make([]localPruneCandidate, 0)

	addCandidate := func(absPath string, info fs.FileInfo) {
		rel, err := filepath.Rel(scopeRoot, absPath)
		if err != nil {
			return
		}
		rel, ok := normalizeLocalRelPath(rel)
		if !ok {
			return
		}
		if !isWithinScope(scopeRoot, absPath) {
			return
		}
		if _, exists := seen[rel]; exists {
			return
		}
		seen[rel] = struct{}{}
		result = append(result, localPruneCandidate{
			AbsPath: absPath,
			RelPath: rel,
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}

	for _, dir := range outputDirs {
		dir = strings.TrimSuffix(strings.TrimSpace(dir), "/")
		if dir == "" {
			continue
		}
		absDir := filepath.Join(scopeRoot, filepath.FromSlash(dir))
		walkErr := filepath.WalkDir(absDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil || !info.Mode().IsRegular() {
				return nil
			}
			addCandidate(path, info)
			return nil
		})
		if walkErr != nil && !os.IsNotExist(walkErr) {
			return nil, walkErr
		}
	}

	if includeArtifacts {
		for relPath := range restorable {
			absPath := filepath.Join(scopeRoot, filepath.FromSlash(relPath))
			if !isWithinScope(scopeRoot, absPath) {
				continue
			}
			info, err := os.Stat(absPath)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			addCandidate(absPath, info)
		}
	}
	return result, nil
}

func normalizeLocalRelPath(p string) (string, bool) {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return "", false
	}
	cleaned := filepath.Clean(filepath.FromSlash(trimmed))
	if cleaned == "." || cleaned == "" {
		return "", false
	}
	slashed := filepath.ToSlash(cleaned)
	if strings.HasPrefix(slashed, "/") || strings.HasPrefix(slashed, "../") || slashed == ".." {
		return "", false
	}
	return slashed, true
}

func isWithinScope(scopeRoot, path string) bool {
	rel, err := filepath.Rel(scopeRoot, path)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, "../"))
}

func passesPruneTimeFilter(modTime time.Time, filter pruneTimeFilter, now time.Time) bool {
	if filter.olderThan > 0 {
		return modTime.Before(now.Add(-filter.olderThan))
	}
	if !filter.since.IsZero() {
		return !modTime.Before(filter.since)
	}
	return true
}

func removeEmptyOutputDirs(scopeRoot string) (int, error) {
	outputDirs := config.ProjectOutputDirs(scopeRoot)
	removed := 0
	for _, dir := range outputDirs {
		dir = strings.TrimSuffix(strings.TrimSpace(dir), "/")
		if dir == "" {
			continue
		}
		root := filepath.Join(scopeRoot, filepath.FromSlash(dir))
		dirs := make([]string, 0)
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				dirs = append(dirs, path)
			}
			return nil
		}); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		sort.Slice(dirs, func(i, j int) bool {
			return len(dirs[i]) > len(dirs[j])
		})
		for _, d := range dirs {
			if err := os.Remove(d); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

func humanizeBytes(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// listJobOutputAssets returns job-output entries from the host_data table for a given job.
// Asset IDs have the format "<jobID>/<relative-path>".
func listJobOutputAssets(database *sql.DB, jobID int64) ([]dataloc.HostDataEntry, error) {
	prefix := fmt.Sprintf("%d/", jobID)
	all, err := dataloc.ListAllAssets(database)
	if err != nil {
		return nil, err
	}
	var results []dataloc.HostDataEntry
	for _, entry := range all {
		if entry.Asset.Kind == dataloc.AssetJobOutput && strings.HasPrefix(entry.Asset.ID, prefix) {
			results = append(results, entry)
		}
	}
	return results, nil
}

// syncJobOutputs rsyncs convention-based output directories from a remote host
// to the local working dir. For cloud jobs, it downloads outputs from R2
// instead. The result count is the number of files downloaded or copied.
//
// Attribution rule (spec: Attribution in specs/job-lifecycle.allium): files
// are registered as a job's outputs only on remote evidence — the runner's
// completion-record listing, or time-window discovery on the execution host.
// Files found in the submitter's LOCAL working tree are never attributed to a
// job (the local tree holds the user's own files and other jobs' synced
// outputs), except when the job actually executed on this machine.
func syncJobOutputs(database *sql.DB, job *db.Job) (artifacts.SyncResult, error) {
	if job.IsLaunchJob() {
		return syncCloudJobOutputsFunc(database, job)
	}

	cloudResult, cloudErr := syncCloudJobOutputsFunc(database, job)
	if cloudErr == nil && cloudResult.Added > 0 {
		return cloudResult, nil
	}
	if cloudErr != nil && !errors.Is(cloudErr, errR2NotConfigured) {
		syncOutputWarnf("warning: job %s: R2 output sync failed (%v); falling back to the host's working tree, which a later job may have overwritten\n",
			ids.FormatJobID(job.ID), cloudErr)
	}

	if !job.HasInventoryHost() || job.WorkingDir == "" {
		return artifacts.SyncResult{}, nil
	}

	localDir := workdir.ResolveLocal(job.WorkingDir)
	if localDir == "" {
		return artifacts.SyncResult{}, nil
	}

	if result, err := syncRecordedJobOutputAssets(database, job); err != nil {
		return artifacts.SyncResult{}, err
	} else if result.Added > 0 {
		return result, nil
	}

	rec := completionRecordFunc(job)

	// Backfill start_time from the host-side completion record when the DB
	// row lacks it. Missing start_time on a completed attempt is itself a bug
	// in the completion sync path (a swallowed metadata read at the recording
	// tick; see internal/ops/sync.go BackfillStartTimeFromCompletionRecord) —
	// recover here so time-window attribution still works for rows closed
	// before that fix.
	if job.StartTime == 0 && rec != nil && rec.StartTime > 0 {
		if err := db.UpdateStartTime(database, job.ID, rec.StartTime); err == nil {
			job.StartTime = rec.StartTime
		}
	}

	lower := outputSyncThreshold(job)
	upper := outputSyncUpperBound(job, rec)
	var outputFiles []runner.OutputFile
	if rec != nil {
		outputFiles = rec.OutputFiles
	}
	if len(outputFiles) == 0 {
		// No runner-attributed file list: fall back to time-window discovery.
		// Discovery needs the attempt's window; a zero start time would match
		// everything in a shared output tree, so refuse rather than guess.
		if job.StartTime == 0 {
			syncOutputWarnf("warning: job %s has no recorded start time; cannot attribute outputs by time window — skipping output discovery\n", ids.FormatJobID(job.ID))
			return artifacts.SyncResult{}, nil
		}
		if isLocalJobHostFunc(job.Host) {
			// The job executed on this machine: the local working tree IS the
			// runner's working tree, so time-window discovery here is
			// first-party evidence, not a guess about the submitter's files.
			files, err := discoverLocalJobOutputFiles(job, localDir, lower, upper)
			if err != nil {
				return artifacts.SyncResult{}, err
			}
			return storeLocalOutputFiles(database, job.ID, localDir, files)
		}
		discovered, err := discoverRemoteJobOutputFilesFunc(job, lower, upper)
		if err != nil {
			return artifacts.SyncResult{}, err
		}
		outputFiles = discovered
	}
	if len(outputFiles) == 0 {
		return artifacts.SyncResult{}, nil
	}
	totalMB := runner.TotalSizeMB(outputFiles)
	if err := syncOutputFilesBackFunc(job.Host, job.WorkingDir, localDir, outputFilePaths(outputFiles), totalMB, 0); err != nil {
		return artifacts.SyncResult{}, err
	}
	// rsync -a preserves mtimes, so the synced copies carry the remote
	// attempt-window timestamps (spec: OutputDiscoveryUsesAttemptStartCutoff).
	sinceFiltered := runner.FilterOutputFilesSince(localDir, outputFiles, lower)
	windowed := runner.FilterOutputFilesUntil(localDir, sinceFiltered, upper)
	if dropped := len(sinceFiltered) - len(windowed); dropped > 0 {
		syncOutputWarnf("warning: job %s: %d output file(s) were modified after the attempt ended (likely overwritten by a later job in the same directory) and were not attributed\n",
			ids.FormatJobID(job.ID), dropped)
	}
	return storeLocalOutputFiles(database, job.ID, localDir, windowed)
}

func syncRecordedJobOutputAssets(database *sql.DB, job *db.Job) (artifacts.SyncResult, error) {
	result := artifacts.SyncResult{}
	entries, err := listJobOutputAssets(database, job.ID)
	if err != nil {
		return result, err
	}
	if len(entries) == 0 {
		return result, nil
	}
	prefix := fmt.Sprintf("%d/", job.ID)
	for _, entry := range entries {
		relPath := strings.TrimPrefix(entry.Asset.ID, prefix)
		relPath = strings.TrimSpace(relPath)
		if relPath == "" || strings.TrimSpace(entry.Path) == "" {
			result.Skipped++
			continue
		}
		remotePath := recordedJobOutputRemotePath(job, entry.Path)
		host := entry.Host
		if host == "" {
			host = job.Host
		}
		copyJob := *job
		copyJob.Host = host
		if err := storeRemoteJobOutputAssetFunc(database, &copyJob, relPath, remotePath, ssh.CopyFromWithRetry); err != nil {
			return result, err
		}
		result.Added++
	}
	return result, nil
}

func recordedJobOutputRemotePath(job *db.Job, recordedPath string) string {
	recordedPath = strings.TrimSpace(recordedPath)
	if strings.HasPrefix(recordedPath, "/") || strings.HasPrefix(recordedPath, "~") {
		return recordedPath
	}
	return artifacts.ResolveRemotePath(job.WorkingDir, recordedPath)
}

// syncCloudJobOutputs downloads convention-based outputs and artifact manifest
// entries from R2 for cloud jobs.
func syncCloudJobOutputs(database *sql.DB, job *db.Job) (artifacts.SyncResult, error) {
	localDir := workdir.ResolveLocal(job.WorkingDir)
	if localDir == "" {
		return artifacts.SyncResult{}, fmt.Errorf("cannot resolve local working directory for job %s", ids.FormatJobID(job.ID))
	}

	cfg, err := config.Load()
	if err != nil {
		return artifacts.SyncResult{}, fmt.Errorf("load config: %w", err)
	}
	r2Client, err := buildR2Client(cfg)
	if err != nil {
		return artifacts.SyncResult{}, fmt.Errorf("create R2 client: %w", err)
	}
	if r2Client == nil {
		return artifacts.SyncResult{}, fmt.Errorf("%w; cannot fetch cloud job outputs", errR2NotConfigured)
	}

	var lastErr error
	syncRun := func(runID int64) artifacts.SyncResult {
		result := artifacts.SyncResult{}
		for _, prefix := range []string{
			r2keys.JobAttemptOutputsPrefix(job.ID, runID),
			r2keys.JobAttemptArtifactFilesPrefix(job.ID, runID),
		} {
			added, err := downloadCloudPrefix(database, r2Client, prefix, localDir, job.ID, runID)
			if err != nil {
				lastErr = err
				continue
			}
			result.Added += added
		}
		return result
	}
	// Discovery-list errors are non-fatal here: probe the recorded runs and
	// download whatever prefixes do list. A missing superseded attempt only
	// costs a re-sync, not a false "absent".
	runIDCandidates, _ := artifactRunIDCandidates(r2Client, job)
	for _, runID := range runIDCandidates {
		if result := syncRun(runID); result.Added > 0 {
			return result, nil
		}
	}
	if lastErr != nil {
		return artifacts.SyncResult{}, lastErr
	}
	return artifacts.SyncResult{}, nil
}

// artifactRunIDCandidates returns the run IDs to probe for a job's R2
// objects: the recorded latest run, legacy run zero, then any runs/
// subdirectories discovered in R2 (superseded attempts whose uploads
// outlived latest_run_id).
//
// The returned error is non-nil when the R2 runs/ discovery listing failed: the
// recorded run IDs are still returned (so probing proceeds), but the candidate
// set may be missing superseded attempts. Callers that must not report a
// confident "absent" propagate this; download/sync callers may ignore it and
// proceed with the recorded runs.
func artifactRunIDCandidates(lister cloudOutputLister, job *db.Job) ([]int64, error) {
	runIDs := jobAttemptRunIDs(job)
	tried := make(map[int64]struct{}, len(runIDs))
	for _, id := range runIDs {
		tried[id] = struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	discovered, err := r2resolve.ListRunIDsFunc(ctx, lister, job.ID)
	cancel()
	if err != nil {
		return runIDs, fmt.Errorf("list R2 runs for job %d: %w", job.ID, err)
	}
	for _, id := range discovered {
		if _, ok := tried[id]; ok {
			continue
		}
		tried[id] = struct{}{}
		runIDs = append(runIDs, id)
	}
	return runIDs, nil
}

// fetchCompletionRecord reads the runner's completion record for a job from
// its host. Returns nil when the record is missing or unreadable.
func fetchCompletionRecord(job *db.Job) *runner.CompletionRecord {
	completionPath := fmt.Sprintf("~/.cache/weft/logs/%d.completion.json", job.ID)
	stdout, _, err := ssh.RunWithTimeout(job.Host, fmt.Sprintf("cat %s 2>/dev/null", completionPath), NormalSyncTimeout)
	if err != nil {
		return nil
	}

	var rec runner.CompletionRecord
	if err := json.Unmarshal([]byte(stdout), &rec); err != nil {
		return nil
	}
	return &rec
}

func outputFilePaths(files []runner.OutputFile) []string {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		if strings.TrimSpace(f.RelPath) == "" {
			continue
		}
		paths = append(paths, filepath.ToSlash(f.RelPath))
	}
	return paths
}

func outputSyncThreshold(job *db.Job) time.Time {
	if job == nil {
		return time.Time{}
	}
	return runner.AttemptOutputThreshold(job.StartTime)
}

// outputSyncUpperBound returns the end-of-attempt cap for discovery-based
// output attribution: end_time + artifacts.AttemptOutputEndSlack when the
// attempt end is known, zero (unbounded) otherwise. The cap narrows the
// cross-attribution window for sibling jobs sharing an output directory
// (spec: OutputDiscoveryUsesAttemptStartCutoff in specs/job-lifecycle.allium).
func outputSyncUpperBound(job *db.Job, rec *runner.CompletionRecord) time.Time {
	var end int64
	if rec != nil && rec.EndTime > 0 {
		end = rec.EndTime
	}
	if end == 0 && job != nil && job.EndTime != nil && *job.EndTime > 0 {
		end = *job.EndTime
	}
	if end == 0 {
		return time.Time{}
	}
	return time.Unix(end, 0).Add(artifacts.AttemptOutputEndSlack)
}

// jobOutputDiscoveryDirs returns the convention output directories for a job.
func jobOutputDiscoveryDirs(job *db.Job) []string {
	if len(job.OutputDirs) > 0 {
		return job.OutputDirs
	}
	return config.DefaultOutputDirs
}

// discoverLocalJobOutputFiles time-window-discovers output files in the local
// working tree. Only valid when the job executed on this machine — for remote
// jobs the local tree holds the submitter's own files, which must never be
// attributed to a job (spec: Attribution).
func discoverLocalJobOutputFiles(job *db.Job, localDir string, lower, upper time.Time) ([]runner.OutputFile, error) {
	files, err := runner.DiscoverJobOutputsSince(localDir, jobOutputDiscoveryDirs(job), job.Outputs, lower)
	if err != nil {
		return nil, err
	}
	return runner.FilterOutputFilesUntil(localDir, files, upper), nil
}

// maxRemoteOutputDiscoveryEntries bounds the remote find so a huge shared
// output tree cannot stall the sync or flood the SSH channel.
const maxRemoteOutputDiscoveryEntries = 20000

// discoverRemoteJobOutputFiles lists files under the job's convention output
// directories (and declared filesystem output refs) inside job.WorkingDir on
// job.Host, keeping only files whose mtime falls inside [lower, upper]. Used
// when the completion record lacks an output_files listing (jobs that
// completed before the runner recorded outputs, or recovery paths). Mtimes
// are compared as unix epochs, so host/submitter timezone skew is irrelevant.
func discoverRemoteJobOutputFiles(job *db.Job, lower, upper time.Time) ([]runner.OutputFile, error) {
	var roots []string
	for _, dir := range jobOutputDiscoveryDirs(job) {
		dir = strings.TrimSuffix(strings.TrimSpace(dir), "/")
		if dir != "" {
			roots = append(roots, dir)
		}
	}
	for _, ref := range job.Outputs {
		if path, ok := runner.FilesystemOutputRefPath(ref); ok {
			if path = strings.TrimSuffix(path, "/"); path != "" {
				roots = append(roots, path)
			}
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}
	quoted := make([]string, 0, len(roots))
	for _, root := range roots {
		quoted = append(quoted, "'"+strings.ReplaceAll(root, "'", `'\''`)+"'")
	}
	// stat -c is GNU coreutils, stat -f the BSD/macOS spelling (same dual
	// form as internal/ops probes).
	remoteCmd := fmt.Sprintf(
		`cd %s 2>/dev/null || exit 0; for d in %s; do [ -e "$d" ] || continue; find "$d" -type f 2>/dev/null; done | head -n %d | while IFS= read -r f; do m=$(stat -c %%Y "$f" 2>/dev/null || stat -f %%m "$f" 2>/dev/null); s=$(stat -c %%s "$f" 2>/dev/null || stat -f %%z "$f" 2>/dev/null); [ -n "$m" ] && [ -n "$s" ] && printf '%%s|%%s|%%s\n' "$m" "$s" "$f"; done`,
		job.WorkingDir, strings.Join(quoted, " "), maxRemoteOutputDiscoveryEntries)
	stdout, _, err := ssh.RunWithTimeout(job.Host, remoteCmd, NormalSyncTimeout)
	if err != nil {
		return nil, fmt.Errorf("discover outputs on %s: %w", job.Host, err)
	}

	var files []runner.OutputFile
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		mtime, mErr := strconv.ParseInt(parts[0], 10, 64)
		size, sErr := strconv.ParseInt(parts[1], 10, 64)
		relPath := strings.TrimPrefix(strings.TrimSpace(parts[2]), "./")
		if mErr != nil || sErr != nil || relPath == "" {
			continue
		}
		at := time.Unix(mtime, 0)
		if !lower.IsZero() && at.Before(lower) {
			continue
		}
		if !upper.IsZero() && at.After(upper) {
			continue
		}
		files = append(files, runner.OutputFile{RelPath: filepath.ToSlash(relPath), SizeBytes: size})
	}
	return files, nil
}

func storeLocalOutputFiles(database *sql.DB, jobID int64, localDir string, files []runner.OutputFile) (artifacts.SyncResult, error) {
	result := artifacts.SyncResult{}
	for _, f := range files {
		relPath := filepath.ToSlash(f.RelPath)
		sourcePath := filepath.Join(localDir, filepath.FromSlash(relPath))
		if err := artifacts.StoreLocalArtifact(database, jobID, relPath, sourcePath); err != nil {
			return result, err
		}
		result.Added++
	}
	return result, nil
}

func downloadCloudPrefix(database *sql.DB, r2Client *r2.Client, prefix, localDir string, jobID, runID int64) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	files, err := r2Client.ListObjects(ctx, prefix)
	cancel()
	if err != nil {
		return 0, err
	}

	added := 0
	for _, f := range files {
		relPath := strings.TrimPrefix(f.Key, prefix)
		relPath = strings.TrimPrefix(relPath, "/")
		if relPath == "" {
			continue
		}
		localPath := filepath.Join(localDir, relPath)
		if _, err := r2Client.DownloadObjectToFileWithIdleTimeout(context.Background(), f.Key, localPath, artifactTimeout); err != nil {
			return added, fmt.Errorf("download %s: %w", f.Key, err)
		}
		if err := artifacts.StoreLocalArtifactForRun(database, jobID, runID, relPath, localPath); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

var (
	syncLocalJobArtifacts            = artifacts.SyncJob
	syncCloudJobArtifactsFunc        = syncCloudJobArtifacts
	syncCloudJobOutputsFunc          = syncCloudJobOutputs
	completionRecordFunc             = fetchCompletionRecord
	discoverRemoteJobOutputFilesFunc = discoverRemoteJobOutputFiles
	isLocalJobHostFunc               = srcsync.IsLocalHost
	syncOutputFilesBackFunc          = srcsync.SyncOutputFilesBack
	syncJobOutputsFunc               = syncJobOutputs
	syncArtifactsForJobFunc          = syncArtifactsForJob
	storeRemoteJobOutputAssetFunc    = artifacts.StoreRemoteArtifact
	syncOutputWarnf                  = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format, args...) }
	buildArtifactR2Client            = func() cloudOutputStore {
		cfg, err := config.Load()
		if err != nil {
			return nil
		}
		client, err := buildR2Client(cfg)
		if err != nil || client == nil {
			// The nil check matters: returning a nil *r2.Client through the
			// interface would make `!= nil` checks pass on a typed nil.
			return nil
		}
		return client
	}
)

// syncArtifactOnDemand materializes a job's artifacts into the local cache
// when a get/cat misses: outputs of jobs on inventory hosts are often only
// pointer records into the host's working directory until a sync copies
// them. Opens its own writable DB handle (get/cat read via OpenForReading).
// Returns the cache entry for token if the sync produced one, nil if the
// sync ran but the token still isn't cached.
func syncArtifactOnDemand(job *db.Job, token string) (*db.Artifact, error) {
	if job == nil || !job.HasInventoryHost() {
		return nil, nil
	}
	database, err := db.Open()
	if err != nil {
		return nil, fmt.Errorf("open database for artifact sync: %w", err)
	}
	defer database.Close()
	if err := syncArtifactsForJobFunc(database, job, nil, NormalSyncTimeout); err != nil {
		return nil, err
	}
	entry, err := db.FindArtifactByNameOrPath(database, job.ID, token)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// artifactNotFoundMessage names everywhere the lookup checked so a missing
// artifact reads as a conclusion, not a shrug. When r2ListFailed is true the
// R2 listing was attempted but errored, so the artifact's absence from R2 is
// unconfirmed — the message says "could not be located" and "status unknown"
// rather than a definitive "not found" (wb40).
func artifactNotFoundMessage(job *db.Job, token string, r2Checked, r2ListFailed bool) string {
	checked := []string{"local cache"}
	if r2Checked {
		if r2ListFailed {
			checked = append(checked, "R2 (listing failed; status unknown)")
		} else {
			checked = append(checked, "R2")
		}
	}
	if job != nil && job.HasInventoryHost() {
		checked = append(checked, job.Host+" via artifact sync")
	}
	verb := "not found"
	if r2ListFailed {
		verb = "could not be located"
	}
	msg := fmt.Sprintf("artifact %q %s for job %s (checked %s)",
		token, verb, ids.FormatJobID(job.ID), strings.Join(checked, ", "))
	if r2ListFailed {
		msg += "; the R2 listing failed, so it may exist in R2 but could not be confirmed"
	}
	if job != nil && job.HasInventoryHost() {
		msg += "; on-prem outputs are not uploaded to R2"
		if locations := inventoryOutputLocations(job); len(locations) > 0 {
			msg += "; expected output locations include " + strings.Join(locations, ", ")
		}
	}
	return msg
}

func writeInventoryArtifactHint(cmd *cobra.Command, job *db.Job) {
	fmt.Fprintln(cmd.OutOrStdout(), "On-prem job outputs are not uploaded to R2.")
	if locations := inventoryOutputLocations(job); len(locations) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Expected output locations include %s.\n", strings.Join(locations, ", "))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Run `weft artifact sync %s` to pull host outputs into the local artifact cache, or inspect the files over SSH.\n", ids.FormatJobID(job.ID))
}

func writeNoCloudArtifactsYetHint(cmd *cobra.Command, job *db.Job) {
	statusLabel := strings.TrimSpace(job.Status)
	if statusLabel == "" {
		statusLabel = job.EffectiveStatus()
	}
	fmt.Fprintln(cmd.OutOrStdout(), "No cached artifacts.")
	fmt.Fprintf(cmd.OutOrStdout(), "Job %s status is %s; artifacts are not available yet. This is normal before the job writes outputs or finishes.\n",
		ids.FormatJobID(job.ID), statusLabel)
}

// writeNoAttributableOutputsHint reports, accurately, that an on-prem job has no
// outputs after `list` already ran the same host discovery that get/cat perform
// on a cache miss. It deliberately omits the "run `weft artifact sync`" advice —
// that sync just ran and found nothing — and instead states the conclusion and
// where to look over SSH.
func writeNoAttributableOutputsHint(cmd *cobra.Command, job *db.Job) {
	fmt.Fprintf(cmd.OutOrStdout(), "No outputs attributable to job %s in its run window.\n", ids.FormatJobID(job.ID))
	if locations := inventoryOutputLocations(job); len(locations) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "Expected output locations: %s.\n", strings.Join(locations, ", "))
		fmt.Fprintln(cmd.OutOrStdout(), "If files exist there, inspect them over SSH; on-prem outputs are not uploaded to R2.")
	}
}

func inventoryOutputLocations(job *db.Job) []string {
	if job == nil || !job.HasInventoryHost() {
		return nil
	}
	root := strings.TrimSpace(job.EffectiveWorkingDir())
	if root == "" {
		return nil
	}
	var rels []string
	if len(job.OutputDirs) > 0 {
		rels = append(rels, job.OutputDirs...)
	} else {
		rels = append(rels, config.DefaultOutputDirs...)
	}
	for _, ref := range job.Outputs {
		if path, ok := runner.FilesystemOutputRefPath(ref); ok {
			rels = append(rels, path)
		}
	}
	seen := map[string]struct{}{}
	var locations []string
	for _, rel := range rels {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		remotePath := artifacts.ResolveRemotePath(root, rel)
		if strings.HasSuffix(rel, "/") && !strings.HasSuffix(remotePath, "/") {
			remotePath += "/"
		}
		location := job.Host + ":" + remotePath
		if _, ok := seen[location]; ok {
			continue
		}
		seen[location] = struct{}{}
		locations = append(locations, location)
		if len(locations) >= 3 {
			break
		}
	}
	return locations
}

func syncArtifactsForJob(database *sql.DB, job *db.Job, r2Client *r2.Client, timeout time.Duration) error {
	if job.IsLaunchJob() {
		_, err := syncCloudJobArtifactsFunc(database, r2Client, job)
		if errors.Is(err, artifacts.ErrManifestMissing) {
			_, err = syncJobOutputsFunc(database, job)
		}
		if err != nil {
			return err
		}
		return nil
	}
	_, err := syncLocalJobArtifacts(database, job, timeout)
	if errors.Is(err, artifacts.ErrManifestMissing) {
		_, err = syncJobOutputsFunc(database, job)
	} else if err != nil && hasFilesystemOutputDeclarations(job) {
		if result, outputErr := syncJobOutputsFunc(database, job); outputErr == nil && result.Added > 0 {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	return nil
}

func hasFilesystemOutputDeclarations(job *db.Job) bool {
	if job == nil {
		return false
	}
	if len(job.OutputDirs) > 0 {
		return true
	}
	for _, ref := range job.Outputs {
		if _, ok := runner.FilesystemOutputRefPath(ref); ok {
			return true
		}
	}
	return false
}

// syncCloudJobArtifacts fetches the artifact manifest from R2, downloads each
// declared artifact into the local artifact store, and registers them in the DB.
func syncCloudJobArtifacts(database *sql.DB, r2Client *r2.Client, job *db.Job) (artifacts.SyncResult, error) {
	return syncCloudJobArtifactsWithStore(database, r2Client, job)
}

type cloudArtifactObjectStore interface {
	GetObject(context.Context, string) ([]byte, error)
	GetObjectReader(context.Context, string) (io.ReadCloser, error)
	ListObjects(context.Context, string) ([]r2.ObjectInfo, error)
}

type progressWriter struct {
	w          io.Writer
	onProgress func()
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 && w.onProgress != nil {
		w.onProgress()
	}
	return n, err
}

func syncCloudJobArtifactsWithStore(database *sql.DB, store cloudArtifactObjectStore, job *db.Job) (artifacts.SyncResult, error) {
	if store == nil {
		return artifacts.SyncResult{}, fmt.Errorf("R2 not configured; cannot fetch cloud job artifacts")
	}

	var (
		manifestData []byte
		runID        int64
		err          error
	)
	foundManifest := false
	candidateRunIDs, _ := artifactRunIDCandidates(store, job)
	for _, candidateRunID := range candidateRunIDs {
		manifestKey := r2keys.JobAttemptArtifactManifest(job.ID, candidateRunID)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		manifestData, err = store.GetObject(ctx, manifestKey)
		cancel()
		if err != nil {
			if r2.IsNotFound(err) {
				continue
			}
			return artifacts.SyncResult{}, fmt.Errorf("fetch manifest from R2: %w", err)
		}
		runID = candidateRunID
		foundManifest = true
		break
	}
	if !foundManifest {
		return artifacts.SyncResult{}, artifacts.ErrManifestMissing
	}

	manifest, err := artifacts.ParseManifest(string(manifestData), job.ID)
	if err != nil {
		return artifacts.SyncResult{}, err
	}

	localRoot, err := artifacts.LocalArtifactsDir()
	if err != nil {
		return artifacts.SyncResult{}, err
	}

	filesPrefix := r2keys.JobAttemptArtifactFilesPrefix(job.ID, runID)
	result := artifacts.SyncResult{}
	for _, spec := range manifest.Artifacts {
		if strings.TrimSpace(spec.Path) == "" {
			result.Skipped++
			continue
		}

		relPath := artifacts.LocalRelativePath(spec.Path)
		storedPath := artifacts.LocalStoredPath(job.ID, spec.Path)
		localPath := filepath.Join(localRoot, storedPath)

		size, sha, err := downloadCloudArtifact(store, filesPrefix, relPath, localPath, artifactTimeout)
		if err != nil {
			return result, fmt.Errorf("download artifact %s: %w", spec.Path, err)
		}

		// Stamp the run whose manifest was found, not latest_run_id
		// (spec: ArtifactRetrievalCoversAllRuns in job-lifecycle.allium).
		if err := db.UpsertArtifact(database, db.Artifact{
			JobID:      job.ID,
			JobRunID:   &runID,
			Name:       spec.Name,
			Path:       spec.Path,
			StoredPath: storedPath,
			SizeBytes:  size,
			SHA256:     sha,
		}); err != nil {
			return result, err
		}
		result.Added++
	}
	return result, nil
}

func jobAttemptRunIDs(job *db.Job) []int64 {
	if job != nil && job.LatestRunID != nil && *job.LatestRunID != 0 {
		return []int64{*job.LatestRunID, 0}
	}
	return []int64{0}
}

func copyCloudObjectToWriterWithIdleTimeout(store cloudArtifactObjectStore, key string, dst io.Writer, idleTimeout time.Duration) (int64, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body, err := store.GetObjectReader(ctx, key)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	if idleTimeout <= 0 {
		return io.Copy(dst, body)
	}

	progressCh := make(chan struct{}, 1)
	timeoutCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(idleTimeout)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-progressCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idleTimeout)
			case <-timer.C:
				timeoutCh <- fmt.Errorf("download stalled: no progress for %s", idleTimeout)
				cancel()
				return
			}
		}
	}()

	pw := &progressWriter{
		w: dst,
		onProgress: func() {
			select {
			case progressCh <- struct{}{}:
			default:
			}
		},
	}

	n, copyErr := io.Copy(pw, body)
	cancel()
	<-done
	select {
	case stallErr := <-timeoutCh:
		return n, stallErr
	default:
	}
	if copyErr != nil {
		return n, copyErr
	}
	return n, nil
}

// downloadCloudObjectToPathAtomic streams an R2 object to dest via a .part
// sibling, teeing into extra when non-nil; dest is renamed into place only
// after a clean close (a failed close leaves incomplete bytes).
func downloadCloudObjectToPathAtomic(store cloudArtifactObjectStore, key, dest string, extra io.Writer, idleTimeout time.Duration) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	tmpDest := dest + ".part"
	f, err := os.Create(tmpDest)
	if err != nil {
		return 0, err
	}
	var w io.Writer = f
	if extra != nil {
		w = io.MultiWriter(f, extra)
	}
	n, copyErr := copyCloudObjectToWriterWithIdleTimeout(store, key, w, idleTimeout)
	closeErr := f.Close()
	if err := cmp.Or(copyErr, closeErr); err != nil {
		_ = os.Remove(tmpDest)
		return 0, err
	}
	if err := os.Rename(tmpDest, dest); err != nil {
		_ = os.Remove(tmpDest)
		return 0, err
	}
	return n, nil
}

func downloadCloudArtifact(store cloudArtifactObjectStore, filesPrefix, relPath, localPath string, idleTimeout time.Duration) (int64, string, error) {
	r2Key := filesPrefix + relPath
	listCtx, listCancel := context.WithTimeout(context.Background(), 30*time.Second)
	objects, err := store.ListObjects(listCtx, r2Key)
	listCancel()
	if err != nil {
		return 0, "", err
	}

	dirPrefix := r2Key + "/"
	isFile := false
	var childKeys []string
	for _, obj := range objects {
		switch {
		case obj.Key == r2Key:
			isFile = true
		case strings.HasPrefix(obj.Key, dirPrefix):
			childKeys = append(childKeys, obj.Key)
		}
	}

	if isFile {
		h := sha256.New()
		n, err := downloadCloudObjectToPathAtomic(store, r2Key, localPath, h, idleTimeout)
		if err != nil {
			return 0, "", err
		}
		return n, fmt.Sprintf("%x", h.Sum(nil)), nil
	}

	if len(childKeys) == 0 {
		return 0, "", fmt.Errorf("%w: artifact object %s not found", r2resolve.ErrArtifactMissing, r2Key)
	}

	if err := os.MkdirAll(localPath, 0o755); err != nil {
		return 0, "", err
	}

	var totalSize int64
	for _, key := range childKeys {
		relChild := strings.TrimPrefix(key, dirPrefix)
		if relChild == "" {
			continue
		}
		childPath := filepath.Join(localPath, relChild)
		n, err := downloadCloudObjectToPathAtomic(store, key, childPath, nil, idleTimeout)
		if err != nil {
			return 0, "", err
		}
		totalSize += n
	}

	return totalSize, "", nil
}

// listCloudJobOutputFiles lists output files for a job by checking R2 for
// both the outputs/ and artifacts/files/ prefixes. When the recorded runs
// yield nothing, it falls back to runs discovered in R2 — a superseded or
// canceled attempt's uploads outlive latest_run_id (see r2resolve.NeedR2Key).
//
// A non-nil error means at least one R2 operation (runs/ discovery or a prefix
// list) failed, so the returned file set may be incomplete — the caller must
// treat this as "cloud status unknown", never "confirmed absent" (wb40). The
// files that did list are still returned.
func listCloudJobOutputFiles(r2Client cloudOutputLister, job *db.Job) ([]runner.OutputFile, error) {
	runIDs, discErr := artifactRunIDCandidates(r2Client, job)
	result, listErr := listCloudOutputFilesForRuns(r2Client, job.ID, runIDs)
	result = dedupeArtifactSpellings(result)
	sort.Slice(result, func(i, j int) bool {
		return result[i].RelPath < result[j].RelPath
	})
	return result, errors.Join(discErr, listErr)
}

func listCloudOutputFilesForRuns(r2Client cloudOutputLister, jobID int64, runIDs []int64) ([]runner.OutputFile, error) {
	result := make([]runner.OutputFile, 0)
	seen := make(map[string]struct{})
	var mu sync.Mutex
	var listErrs []error
	appendFile := func(relPath string, sizeBytes int64, r2Key string) {
		if relPath == "" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := seen[relPath]; ok {
			return
		}
		seen[relPath] = struct{}{}
		result = append(result, runner.OutputFile{
			RelPath:   relPath,
			SizeBytes: sizeBytes,
			R2Key:     r2Key,
		})
	}
	recordErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		listErrs = append(listErrs, err)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for _, runID := range runIDs {
		// Convention-based outputs
		outputsPrefix := r2keys.JobAttemptOutputsPrefix(jobID, runID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			files, err := r2Client.ListObjects(ctx, outputsPrefix)
			cancel()
			if err != nil {
				recordErr(fmt.Errorf("list %s: %w", outputsPrefix, err))
				return
			}
			for _, f := range files {
				appendFile(strings.TrimPrefix(f.Key, outputsPrefix), f.SizeBytes, f.Key)
			}
		}()

		// Artifact manifest entries
		artifactsPrefix := r2keys.JobAttemptArtifactFilesPrefix(jobID, runID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			aFiles, err := r2Client.ListObjects(ctx, artifactsPrefix)
			cancel()
			if err != nil {
				recordErr(fmt.Errorf("list %s: %w", artifactsPrefix, err))
				return
			}
			for _, f := range aFiles {
				appendFile("artifacts/"+strings.TrimPrefix(f.Key, artifactsPrefix), f.SizeBytes, f.Key)
			}
		}()
	}
	wg.Wait()
	return result, errors.Join(listErrs...)
}

// dedupeArtifactSpellings collapses the dual upload of declared artifacts
// that live under the convention output dirs: the same payload appears under
// both the outputs prefix ("output/f") and the artifact-files prefix
// (displayed "artifacts/output/f"). When sizes agree, keep one row,
// preferring the plain spelling. Size disagreement means genuinely different
// objects (e.g. a literal artifacts/ directory in the working dir), so both
// rows are kept.
func dedupeArtifactSpellings(files []runner.OutputFile) []runner.OutputFile {
	out := make([]runner.OutputFile, 0, len(files))
	canonIdx := make(map[string]int, len(files))
	for _, f := range files {
		canon := canonicalArtifactRelPath(f.RelPath)
		i, ok := canonIdx[canon]
		if !ok {
			canonIdx[canon] = len(out)
			out = append(out, f)
			continue
		}
		if out[i].SizeBytes == f.SizeBytes {
			if strings.HasPrefix(out[i].RelPath, "artifacts/") && !strings.HasPrefix(f.RelPath, "artifacts/") {
				out[i] = f
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

// recordJobOutputAssets records discovered output files as job-output assets in the host_data table.
func recordJobOutputAssets(database *sql.DB, jobID int64, host string, files []runner.OutputFile) error {
	for _, f := range files {
		assetID := fmt.Sprintf("%d/%s", jobID, f.RelPath)
		entry := dataloc.HostDataEntry{
			Host: host,
			Asset: dataloc.DataAsset{
				Kind: dataloc.AssetJobOutput,
				ID:   assetID,
			},
			Path:      f.RelPath,
			SizeBytes: f.SizeBytes,
			LastSeen:  time.Now(),
		}
		if err := dataloc.RecordAsset(database, entry); err != nil {
			return err
		}
	}
	return nil
}
