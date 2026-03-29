package cmd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
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
}

var artifactSyncCmd = &cobra.Command{
	Use:   "sync [job-id]...",
	Short: "Sync artifacts into the local store",
	Args:  usageArgs(cobra.MinimumNArgs(0)),
	RunE:  runArtifactSync,
}

var artifactListCmd = &cobra.Command{
	Use:   "list <job-id>...",
	Short: "List cached artifacts for a job",
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runArtifactList,
}

var artifactGetCmd = &cobra.Command{
	Use:   "get <job-id>... <name-or-path>",
	Short: "Retrieve a cached artifact",
	Long: `Retrieve an artifact from the local cache.

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

var artifactCatCmd = &cobra.Command{
	Use:   "cat <job-id> <name-or-path>",
	Short: "Write a cached artifact to stdout",
	Long: `Write an artifact from the local cache to stdout.

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
)

func init() {
	rootCmd.AddCommand(artifactCmd)
	artifactCmd.AddCommand(artifactSyncCmd)
	artifactCmd.AddCommand(artifactListCmd)
	artifactCmd.AddCommand(artifactGetCmd)
	artifactCmd.AddCommand(artifactCatCmd)
	artifactCmd.AddCommand(artifactAddCmd)

	addArtifactListFlags(artifactListCmd)
	artifactGetCmd.Flags().StringVarP(&artifactOutput, "output", "o", "", "Output path (default: current directory, use '-' for stdout)")
	artifactGetCmd.Flags().BoolVar(&artifactAll, "all", false, "Download all artifacts for the job")
	artifactGetCmd.Flags().StringSliceVar(&artifactTag, "tag", nil, "Resolve job ID by tag (can be repeated)")
	artifactGetCmd.Flags().BoolVar(&artifactLatest, "latest", false, "Use the latest job when resolving by tag")
	artifactAddCmd.Flags().StringVar(&artifactName, "name", "", "Optional artifact name")
	artifactCatCmd.Flags().StringSliceVar(&artifactTag, "tag", nil, "Resolve job ID by tag (can be repeated)")
	artifactCatCmd.Flags().BoolVar(&artifactLatest, "latest", false, "Use the latest job when resolving by tag")
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
			errorsList = append(errorsList, fmt.Sprintf("job %d: get job: %v", jobID, err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d not found", jobID))
			continue
		}
		if job.IsLaunchJob() {
			result, syncErr := syncCloudJobArtifacts(database, r2Client, job)
			if syncErr != nil {
				if errors.Is(syncErr, artifacts.ErrManifestMissing) {
					// No manifest in R2; try convention-based output sync
					if outErr := syncJobOutputs(job); outErr == nil {
						fmt.Fprintf(cmd.OutOrStdout(), "Job %d: synced convention-based outputs\n", jobID)
						continue
					}
					errorsList = append(errorsList, fmt.Sprintf("artifact manifest not found for job %d", jobID))
					continue
				}
				errorsList = append(errorsList, fmt.Sprintf("job %d: sync cloud artifacts: %v", jobID, syncErr))
				continue
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Job %d: synced %d artifacts from R2 (skipped %d)\n", jobID, result.Added, result.Skipped)
			continue
		}
		result, err := artifacts.SyncJob(database, job, NormalSyncTimeout)
		if err != nil {
			if errors.Is(err, artifacts.ErrManifestMissing) {
				// Try convention-based output sync instead
				if syncErr := syncJobOutputs(job); syncErr == nil {
					fmt.Fprintf(cmd.OutOrStdout(), "Job %d: synced convention-based outputs\n", jobID)
					continue
				}
				errorsList = append(errorsList, fmt.Sprintf("artifact manifest not found for job %d", jobID))
				continue
			}
			if ssh.IsConnectionError(err.Error()) {
				errorsList = append(errorsList, fmt.Sprintf("host %s unreachable while syncing artifacts for job %d", job.Host, jobID))
				continue
			}
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		if len(jobIDs) > 1 {
			fmt.Fprintf(cmd.OutOrStdout(), "Job %d: synced %d artifacts (skipped %d)\n", jobID, result.Added, result.Skipped)
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
				fmt.Fprintf(os.Stderr, "Warning: failed to sync cloud artifacts for job %d: %v\n", job.ID, syncErr)
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
				fmt.Fprintf(os.Stderr, "Warning: host %s unreachable while syncing artifacts for job %d\n", job.Host, job.ID)
				continue
			}
			fmt.Fprintf(os.Stderr, "Warning: failed to sync artifacts for job %d: %v\n", job.ID, err)
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
	database, err := db.Open()
	if err != nil {
		return err
	}
	defer database.Close()

	// Build R2 client once for cloud job listing (best-effort, nil if unconfigured)
	var r2Client *r2.Client
	if cfg, err := config.Load(); err == nil {
		r2Client, _ = buildR2Client(cfg)
	}

	var errorsList []string
	for i, jobID := range jobIDs {
		if len(jobIDs) > 1 {
			if i > 0 {
				fmt.Fprintln(cmd.OutOrStdout())
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Job %d:\n", jobID)
		}

		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: get job: %v", jobID, err))
			continue
		}
		if job == nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d not found", jobID))
			continue
		}

		if artifactListSync {
			if _, err := artifacts.SyncJob(database, job, NormalSyncTimeout); err != nil && !errors.Is(err, artifacts.ErrManifestMissing) {
				errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
				continue
			}
		}

		entries, err := db.ListArtifactsByJob(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}

		// Also show job-output assets from host_data
		outputAssets, _ := listJobOutputAssets(database, jobID)

		// For cloud jobs, also list output files from R2 and warn on upload failures.
		var cloudOutputFiles []runner.OutputFile
		if job.IsLaunchJob() && r2Client != nil {
			cloudOutputFiles = listCloudJobOutputFiles(r2Client, job)
		}
		if job.IsLaunchJob() {
			warnOnFailedCloudOutputUpload(cmd, job.ID)
		}

		if len(entries) == 0 && len(outputAssets) == 0 && len(cloudOutputFiles) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No cached artifacts.")
			continue
		}
		for _, entry := range entries {
			name := entry.Name
			if name == "" {
				name = "-"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d\t%s\n", name, entry.Path, entry.SizeBytes, entry.SHA256)
		}
		for _, a := range outputAssets {
			fmt.Fprintf(cmd.OutOrStdout(), "output\t%s\t%d\t%s\n", a.Path, a.SizeBytes, a.Host)
		}
		for _, f := range cloudOutputFiles {
			fmt.Fprintf(cmd.OutOrStdout(), "output\t%s\t%d\tcloud\n", f.RelPath, f.SizeBytes)
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
	completionPath := filepath.Join(home, ".cache", "weft", "logs", fmt.Sprintf("%d.completion.json", jobID))
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
	if rec.OutputUpload.Status == "failed" || rec.OutputUpload.Status == "partial" {
		fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: Output upload failed — outputs were not uploaded to R2 (disk may have been full)")
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
	database, err := db.Open()
	if err != nil {
		return err
	}
	defer database.Close()

	var r2Client *r2.Client
	if cfg, err := config.Load(); err == nil {
		r2Client, _ = buildR2Client(cfg)
	}

	multiple := len(jobIDs) > 1
	var errorsList []string
	for _, jobID := range jobIDs {
		entry, err := db.FindArtifactByNameOrPath(database, jobID, token)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows) {
				// Fall back to R2 download for cloud jobs
				job, jobErr := db.GetJobByID(database, jobID)
				if jobErr == nil && job != nil && job.IsLaunchJob() && r2Client != nil {
					if dlErr := fetchCloudArtifactByToken(cmd, r2Client, job, token, multiple); dlErr == nil {
						continue
					}
				}
				errorsList = append(errorsList, fmt.Sprintf("artifact %q not found for job %d", token, jobID))
				continue
			}
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}

		localPath, err := artifacts.LocalPathFromStored(entry.StoredPath)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		dest, err := resolveArtifactOutputPathForJob(localPath, artifactOutput, jobID, multiple)
		if err != nil {
			return err
		}
		if dest == "-" {
			if err := copyToWriter(localPath, cmd.OutOrStdout()); err != nil {
				errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			}
			continue
		}
		if err := copyFile(localPath, dest); err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", dest)
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

// fetchCloudArtifactByToken searches R2 cloud outputs for a file matching token
// (by basename or path suffix) and downloads it.
func fetchCloudArtifactByToken(cmd *cobra.Command, r2Client *r2.Client, job *db.Job, token string, multiple bool) error {
	cloudFiles := listCloudJobOutputFiles(r2Client, job)
	for _, f := range cloudFiles {
		basename := filepath.Base(f.RelPath)
		if basename == token || f.RelPath == token || strings.HasSuffix(f.RelPath, "/"+token) {
			return downloadSingleCloudFile(cmd, r2Client, job, f, multiple)
		}
	}
	return fmt.Errorf("not found in cloud outputs")
}

// downloadSingleCloudFile downloads one cloud output file to the output destination.
func downloadSingleCloudFile(cmd *cobra.Command, r2Client *r2.Client, job *db.Job, f runner.OutputFile, multiple bool) error {
	runID := int64(0)
	if job.LatestRunID != nil {
		runID = *job.LatestRunID
	}

	// Determine the R2 key based on whether this is an artifact or convention output
	var r2Key string
	if strings.HasPrefix(f.RelPath, "artifacts/") {
		trimmed := strings.TrimPrefix(f.RelPath, "artifacts/")
		r2Key = r2keys.JobAttemptArtifactFilesPrefix(job.ID, runID) + trimmed
	} else {
		r2Key = r2keys.JobAttemptOutputsPrefix(job.ID, runID) + f.RelPath
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	data, err := r2Client.GetObject(ctx, r2Key)
	if err != nil {
		return fmt.Errorf("download from R2: %w", err)
	}

	dest, err := resolveArtifactOutputPathForJob(f.RelPath, artifactOutput, job.ID, multiple)
	if err != nil {
		return err
	}
	if dest == "-" {
		_, err := cmd.OutOrStdout().Write(data)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", dest)
	return nil
}

// downloadCloudOutputFiles downloads all cloud output files for a job.
func downloadCloudOutputFiles(cmd *cobra.Command, r2Client *r2.Client, job *db.Job, files []runner.OutputFile) (int, error) {
	multiple := len(files) > 1
	downloaded := 0
	for _, f := range files {
		if err := downloadSingleCloudFile(cmd, r2Client, job, f, multiple); err != nil {
			return downloaded, fmt.Errorf("download %s: %w", f.RelPath, err)
		}
		downloaded++
	}
	return downloaded, nil
}

func fetchAllArtifactsForJobs(cmd *cobra.Command, jobIDs []int64) error {
	database, err := db.Open()
	if err != nil {
		return err
	}
	defer database.Close()

	var r2Client *r2.Client
	if cfg, err := config.Load(); err == nil {
		r2Client, _ = buildR2Client(cfg)
	}

	var errorsList []string
	for _, jobID := range jobIDs {
		entries, err := db.ListArtifactsByJob(database, jobID)
		if err != nil {
			errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, err))
			continue
		}

		if len(entries) == 0 {
			// Try cloud outputs for launch jobs
			job, jobErr := db.GetJobByID(database, jobID)
			if jobErr == nil && job != nil && job.IsLaunchJob() && r2Client != nil {
				cloudFiles := listCloudJobOutputFiles(r2Client, job)
				if len(cloudFiles) > 0 {
					downloaded, dlErr := downloadCloudOutputFiles(cmd, r2Client, job, cloudFiles)
					if dlErr != nil {
						errorsList = append(errorsList, fmt.Sprintf("job %d: %v", jobID, dlErr))
					}
					if downloaded == 0 && dlErr == nil {
						fmt.Fprintf(cmd.OutOrStdout(), "Job %d: no artifacts found\n", jobID)
					}
					continue
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Job %d: no artifacts found\n", jobID)
			continue
		}

		for _, entry := range entries {
			localPath, err := artifacts.LocalPathFromStored(entry.StoredPath)
			if err != nil {
				errorsList = append(errorsList, fmt.Sprintf("job %d artifact %q: %v", jobID, entry.Path, err))
				continue
			}
			dest, err := resolveArtifactOutputPathForJob(localPath, artifactOutput, jobID, len(jobIDs) > 1)
			if err != nil {
				return err
			}
			if err := copyFile(localPath, dest); err != nil {
				errorsList = append(errorsList, fmt.Sprintf("job %d artifact %q: %v", jobID, entry.Path, err))
				continue
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", dest)
		}
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
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

	database, err := db.Open()
	if err != nil {
		return err
	}
	defer database.Close()

	entry, err := db.FindArtifactByNameOrPath(database, jobID, token)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("artifact %q not found for job %d", token, jobID)
		}
		return err
	}

	localPath, err := artifacts.LocalPathFromStored(entry.StoredPath)
	if err != nil {
		return err
	}
	return copyToWriter(localPath, cmd.OutOrStdout())
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
		return fmt.Errorf("job %d not found", jobID)
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
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, usageErrorf("invalid job id %q", raw)
	}
	return id, nil
}

func resolveArtifactJobID(args []string) (int64, error) {
	if len(artifactTag) == 0 {
		return parseJobID(args[0])
	}

	database, err := db.Open()
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

func copyFile(src, dest string) error {
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

// syncJobOutputs rsyncs convention-based output directories from a remote host to the local working dir.
// For cloud jobs, it downloads outputs from R2 instead.
func syncJobOutputs(job *db.Job) error {
	if job.IsLaunchJob() {
		return syncCloudJobOutputs(job)
	}

	if !job.HasInventoryHost() || job.WorkingDir == "" {
		return nil
	}

	// Read completion record from remote to get output files
	completionPath := fmt.Sprintf("~/.cache/weft/logs/%d.completion.json", job.ID)
	stdout, _, err := ssh.RunWithTimeout(job.Host, fmt.Sprintf("cat %s 2>/dev/null", completionPath), NormalSyncTimeout)
	if err != nil {
		return nil
	}

	var rec runner.CompletionRecord
	if err := json.Unmarshal([]byte(stdout), &rec); err != nil {
		return nil
	}

	if len(rec.OutputFiles) == 0 {
		return nil
	}

	// Extract unique top-level directories from output file paths
	dirSet := map[string]bool{}
	for _, f := range rec.OutputFiles {
		topDir := strings.SplitN(f.RelPath, "/", 2)[0]
		dirSet[topDir] = true
	}
	dirs := make([]string, 0, len(dirSet))
	for d := range dirSet {
		dirs = append(dirs, d)
	}

	localDir := workdir.ResolveLocal(job.WorkingDir)
	if localDir == "" {
		return nil
	}

	totalMB := runner.TotalSizeMB(rec.OutputFiles)
	return srcsync.SyncOutputsBack(job.Host, job.WorkingDir, localDir, dirs, totalMB, 0)
}

// syncCloudJobOutputs downloads convention-based outputs and artifact manifest
// entries from R2 for cloud jobs.
func syncCloudJobOutputs(job *db.Job) error {
	localDir := workdir.ResolveLocal(job.WorkingDir)
	if localDir == "" {
		return fmt.Errorf("cannot resolve local working directory for job %d", job.ID)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	r2Client, err := buildR2Client(cfg)
	if err != nil {
		return fmt.Errorf("create R2 client: %w", err)
	}
	if r2Client == nil {
		return fmt.Errorf("R2 not configured; cannot fetch cloud job outputs")
	}

	runID := int64(0)
	if job.LatestRunID != nil {
		runID = *job.LatestRunID
	}

	// Download convention-based outputs
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	outputsPrefix := r2keys.JobAttemptOutputsPrefix(job.ID, runID)
	if err := r2Client.DownloadResults(ctx, outputsPrefix, localDir); err != nil {
		return err
	}

	// Download artifact manifest entries (files declared via WEFT_ARTIFACT_MANIFEST)
	artifactsPrefix := r2keys.JobAttemptArtifactFilesPrefix(job.ID, runID)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	return r2Client.DownloadResults(ctx2, artifactsPrefix, localDir)
}

// syncCloudJobArtifacts fetches the artifact manifest from R2, downloads each
// declared artifact into the local artifact store, and registers them in the DB.
func syncCloudJobArtifacts(database *sql.DB, r2Client *r2.Client, job *db.Job) (artifacts.SyncResult, error) {
	if r2Client == nil {
		return artifacts.SyncResult{}, fmt.Errorf("R2 not configured; cannot fetch cloud job artifacts")
	}

	runID := int64(0)
	if job.LatestRunID != nil {
		runID = *job.LatestRunID
	}

	manifestKey := r2keys.JobAttemptArtifactManifest(job.ID, runID)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manifestData, err := r2Client.GetObject(ctx, manifestKey)
	if err != nil {
		if r2.IsNotFound(err) {
			return artifacts.SyncResult{}, artifacts.ErrManifestMissing
		}
		return artifacts.SyncResult{}, fmt.Errorf("fetch manifest from R2: %w", err)
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

		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return result, err
		}

		r2Key := filesPrefix + relPath
		dlCtx, dlCancel := context.WithTimeout(context.Background(), 60*time.Second)
		data, dlErr := r2Client.GetObject(dlCtx, r2Key)
		dlCancel()
		if dlErr != nil {
			return result, fmt.Errorf("download artifact %s: %w", spec.Path, dlErr)
		}

		if err := os.WriteFile(localPath, data, 0o644); err != nil {
			return result, err
		}

		size := int64(len(data))
		sha := fmt.Sprintf("%x", sha256.Sum256(data))

		if err := db.UpsertArtifact(database, db.Artifact{
			JobID:      job.ID,
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

// listCloudJobOutputFiles lists output files for a cloud job by checking R2 for
// both the outputs/ and artifacts/files/ prefixes.
func listCloudJobOutputFiles(r2Client *r2.Client, job *db.Job) []runner.OutputFile {
	runID := int64(0)
	if job.LatestRunID != nil {
		runID = *job.LatestRunID
	}

	var result []runner.OutputFile

	// Convention-based outputs
	outputsPrefix := r2keys.JobAttemptOutputsPrefix(job.ID, runID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	files, err := r2Client.ListObjects(ctx, outputsPrefix)
	cancel()
	if err == nil {
		for _, f := range files {
			relPath := strings.TrimPrefix(f.Key, outputsPrefix)
			if relPath == "" {
				continue
			}
			result = append(result, runner.OutputFile{
				RelPath:   relPath,
				SizeBytes: f.SizeBytes,
			})
		}
	}

	// Artifact manifest entries
	artifactsPrefix := r2keys.JobAttemptArtifactFilesPrefix(job.ID, runID)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	aFiles, err := r2Client.ListObjects(ctx2, artifactsPrefix)
	cancel2()
	if err == nil {
		for _, f := range aFiles {
			relPath := strings.TrimPrefix(f.Key, artifactsPrefix)
			if relPath == "" {
				continue
			}
			result = append(result, runner.OutputFile{
				RelPath:   "artifacts/" + relPath,
				SizeBytes: f.SizeBytes,
			})
		}
	}

	return result
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
