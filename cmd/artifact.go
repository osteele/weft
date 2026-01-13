package cmd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/artifacts"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var artifactCmd = &cobra.Command{
	Use:   "artifact",
	Short: "Manage job artifacts",
	Long: `Manage job artifacts produced by jobs.

Artifacts are declared by writing a manifest on the remote host and then
synced into a durable local store for retrieval.`,
}

var artifactSyncCmd = &cobra.Command{
	Use:   "sync <job-id>",
	Short: "Sync artifacts for a job into the local store",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runArtifactSync,
}

var artifactListCmd = &cobra.Command{
	Use:   "list <job-id>",
	Short: "List cached artifacts for a job",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runArtifactList,
}

var artifactGetCmd = &cobra.Command{
	Use:   "get <job-id> <name-or-path>",
	Short: "Retrieve a cached artifact",
	Long: `Retrieve an artifact from the local cache.

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
	RunE: runArtifactGet,
}

var artifactAddCmd = &cobra.Command{
	Use:   "add <job-id> <path>",
	Short: "Add an artifact entry to the remote manifest",
	Args:  usageArgs(cobra.ExactArgs(2)),
	RunE:  runArtifactAdd,
}

var (
	artifactListSync bool
	artifactOutput   string
	artifactName     string
	artifactTag      []string
	artifactLatest   bool
)

func init() {
	rootCmd.AddCommand(artifactCmd)
	artifactCmd.AddCommand(artifactSyncCmd)
	artifactCmd.AddCommand(artifactListCmd)
	artifactCmd.AddCommand(artifactGetCmd)
	artifactCmd.AddCommand(artifactAddCmd)

	artifactListCmd.Flags().BoolVar(&artifactListSync, "sync", false, "Sync artifacts from remote before listing")
	artifactGetCmd.Flags().StringVarP(&artifactOutput, "output", "o", "", "Output path (default: current directory)")
	artifactGetCmd.Flags().StringSliceVar(&artifactTag, "tag", nil, "Resolve job ID by tag (can be repeated)")
	artifactGetCmd.Flags().BoolVar(&artifactLatest, "latest", false, "Use the latest job when resolving by tag")
	artifactAddCmd.Flags().StringVar(&artifactName, "name", "", "Optional artifact name")
}

func runArtifactSync(cmd *cobra.Command, args []string) error {
	jobID, err := parseJobID(args[0])
	if err != nil {
		return err
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
	result, err := artifacts.SyncJob(database, job, NormalSyncTimeout)
	if err != nil {
		if errors.Is(err, artifacts.ErrManifestMissing) {
			return fmt.Errorf("artifact manifest not found for job %d", jobID)
		}
		if ssh.IsConnectionError(err.Error()) {
			return fmt.Errorf("host %s unreachable while syncing artifacts", job.Host)
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Synced %d artifacts (skipped %d)\n", result.Added, result.Skipped)
	return nil
}

func runArtifactList(cmd *cobra.Command, args []string) error {
	jobID, err := parseJobID(args[0])
	if err != nil {
		return err
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

	if artifactListSync {
		if _, err := artifacts.SyncJob(database, job, NormalSyncTimeout); err != nil && !errors.Is(err, artifacts.ErrManifestMissing) {
			return err
		}
	}

	entries, err := db.ListArtifactsByJob(database, jobID)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No cached artifacts.")
		return nil
	}
	for _, entry := range entries {
		name := entry.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d\t%s\n", name, entry.Path, entry.SizeBytes, entry.SHA256)
	}
	return nil
}

func runArtifactGet(cmd *cobra.Command, args []string) error {
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
	dest, err := resolveArtifactOutputPath(localPath, artifactOutput)
	if err != nil {
		return err
	}
	if err := copyFile(localPath, dest); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", dest)
	return nil
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
	info, err := os.Stat(output)
	if err == nil && info.IsDir() {
		return filepath.Join(output, filepath.Base(source)), nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return output, nil
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
