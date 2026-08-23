package cmd

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2keys"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/spf13/cobra"
)

var sourceCmd = &cobra.Command{
	Use:   "source",
	Short: "Inspect job source snapshots and execution provenance",
	Long: `Inspect the immutable source closure submitted for a job attempt.

Examples:
	weft source inspect wj1443
	weft source inspect wj1443 --json
  weft source ls wj1443
  weft source ls wj1443 scripts/
  weft source cat wj1443
  weft source cat wj1443 scripts/train.py
  weft source cat wj1443 --attempt 4 scripts/train.py`,
}

var sourceLsCmd = &cobra.Command{
	Use:     "ls <job-id> [prefix]",
	Aliases: []string{"list"},
	Short:   "List files in a job source snapshot",
	Args:    usageArgs(cobra.RangeArgs(1, 2)),
	RunE:    runSourceLs,
}

var sourceCatCmd = &cobra.Command{
	Use:   "cat <job-id> [path]",
	Short: "Print a file from a job source snapshot",
	Args:  usageArgs(cobra.RangeArgs(1, 2)),
	RunE:  runSourceCat,
}

var sourceDiffCmd = &cobra.Command{
	Use:   "diff <job-a> <job-b> [path]",
	Short: "Diff two job source snapshots",
	Args:  usageArgs(cobra.RangeArgs(2, 3)),
	RunE:  runSourceDiff,
}

var sourceInspectCmd = &cobra.Command{
	Use:   "inspect <job-id>",
	Short: "Inspect submitted and executed source identities",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runSourceInspect,
}

var (
	sourceAttempt  int
	sourceAttemptA int
	sourceAttemptB int
	sourceJSON     bool
)

const sourceCmdTimeout = 2 * time.Minute

func init() {
	rootCmd.AddCommand(sourceCmd)
	sourceCmd.AddCommand(sourceLsCmd)
	sourceCmd.AddCommand(sourceCatCmd)
	sourceCmd.AddCommand(sourceDiffCmd)
	sourceCmd.AddCommand(sourceInspectCmd)
	addSourceAttemptFlag(sourceLsCmd)
	addSourceAttemptFlag(sourceCatCmd)
	addSourceAttemptFlag(sourceInspectCmd)
	sourceInspectCmd.Flags().BoolVar(&sourceJSON, "json", false, "Emit versioned JSON")
	sourceDiffCmd.Flags().IntVar(&sourceAttemptA, "attempt-a", 0, "Use a specific attempt number for the first job (default: latest)")
	sourceDiffCmd.Flags().IntVar(&sourceAttemptB, "attempt-b", 0, "Use a specific attempt number for the second job (default: latest)")
}

func addSourceAttemptFlag(cmd *cobra.Command) {
	cmd.Flags().IntVar(&sourceAttempt, "attempt", 0, "Use a specific attempt number (default: latest)")
}

type sourceObjectStore interface {
	GetObject(context.Context, string) ([]byte, error)
	GetObjectReader(context.Context, string) (io.ReadCloser, error)
}

var newSourceStore = func() (sourceObjectStore, error) {
	return newR2Client()
}

type sourceMapping struct {
	R2Key     string
	RemoteDir string
}

type resolvedJobSource struct {
	Job      *db.Job
	Attempt  db.JobAttempt
	Mapping  sourceMapping
	Manifest *cloud.AgentJob
}

func runSourceLs(cmd *cobra.Command, args []string) error {
	jobID, err := parseSingleJobID(args[0])
	if err != nil {
		return err
	}
	prefix := ""
	if len(args) == 2 {
		prefix = args[1]
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	_, attempt, sourceMeta, err := lookupJobSourceMetadata(database, jobID, sourceAttempt)
	if err != nil {
		return err
	}
	if sourceMeta != nil && sourceMeta.Pin != nil {
		client, err := newSourceStore()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), sourceCmdTimeout)
		defer cancel()
		return listPinnedSource(ctx, client, sourceMeta.Pin, prefix, cmd.OutOrStdout())
	}
	if attempt.LaunchID == nil {
		return fmt.Errorf("job %s attempt #%d has no immutable source inventory; legacy rsync attempts cannot be listed", ids.FormatJobID(jobID), attempt.AttemptNumber)
	}

	client, err := newSourceStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sourceCmdTimeout)
	defer cancel()

	source, err := resolveJobSource(ctx, database, client, jobID, sourceAttempt)
	if err != nil {
		return err
	}
	body, err := client.GetObjectReader(ctx, source.Mapping.R2Key)
	if err != nil {
		return fmt.Errorf("fetch source tarball from R2 key %s: %w", source.Mapping.R2Key, err)
	}
	defer body.Close()
	return listSourceTarball(body, prefix, cmd.OutOrStdout())
}

type sourceIdentityView struct {
	Kind   string `json:"identity_kind"`
	SHA256 string `json:"sha256"`
}

type sourceRootView struct {
	MountRel      string `json:"mount_rel"`
	MountBasename string `json:"mount_basename"`
	SHA256        string `json:"sha256"`
	R2Key         string `json:"r2_key"`
	SizeBytes     int64  `json:"size_bytes,omitempty"`
	BlobCount     int    `json:"blob_count,omitempty"`
}

type sourceInspection struct {
	APIVersion string                         `json:"api_version"`
	JobID      string                         `json:"job_id"`
	Attempt    int                            `json:"attempt"`
	Verdict    string                         `json:"verdict"`
	Submitted  *sourceIdentityView            `json:"submitted_source,omitempty"`
	Execution  *db.JobSourceExecutionMetadata `json:"execution_source,omitempty"`
	Roots      []sourceRootView               `json:"roots,omitempty"`
}

func runSourceInspect(cmd *cobra.Command, args []string) error {
	jobID, err := parseSingleJobID(args[0])
	if err != nil {
		return err
	}
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	_, attempt, source, err := lookupJobSourceMetadata(database, jobID, sourceAttempt)
	if err != nil {
		return err
	}
	view := inspectSourceMetadata(jobID, attempt, source)
	if sourceJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(view)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Job:                %s attempt #%d\n", view.JobID, view.Attempt)
	fmt.Fprintf(cmd.OutOrStdout(), "Verdict:            %s\n", view.Verdict)
	if view.Submitted != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Submitted closure:  %s %s\n", view.Submitted.Kind, view.Submitted.SHA256)
	}
	if view.Execution != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Dispatch mode:      %s\n", view.Execution.DispatchMode)
		fmt.Fprintf(cmd.OutOrStdout(), "Executed identity:  %s %s\n", view.Execution.IdentityKind, view.Execution.DispatchedSHA256)
		fmt.Fprintf(cmd.OutOrStdout(), "Agent verification: %s", view.Execution.Verification)
		if view.Execution.VerifiedSHA256 != "" {
			fmt.Fprintf(cmd.OutOrStdout(), " %s", view.Execution.VerifiedSHA256)
		}
		if view.Execution.AgentVersion != "" {
			fmt.Fprintf(cmd.OutOrStdout(), " (agent %s)", view.Execution.AgentVersion)
		}
		fmt.Fprintln(cmd.OutOrStdout())
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Roots:              %d\n", len(view.Roots))
	for _, root := range view.Roots {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s  %s\n", root.MountRel, root.SHA256, root.R2Key)
	}
	return nil
}

func lookupJobSourceMetadata(database *sql.DB, jobID int64, attemptNumber int) (*db.Job, db.JobAttempt, *db.JobSourceMetadata, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return nil, db.JobAttempt{}, nil, fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
	}
	if job == nil {
		return nil, db.JobAttempt{}, nil, fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	attempt, err := selectSourceAttempt(database, jobID, attemptNumber)
	if err != nil {
		return nil, db.JobAttempt{}, nil, err
	}
	if attempt.Metadata != nil && attempt.Metadata.Source != nil {
		return job, attempt, attempt.Metadata.Source, nil
	}
	if attemptNumber == 0 && job.Metadata != nil {
		return job, attempt, job.Metadata.Source, nil
	}
	return job, attempt, nil, nil
}

func inspectSourceMetadata(jobID int64, attempt db.JobAttempt, source *db.JobSourceMetadata) sourceInspection {
	view := sourceInspection{APIVersion: "weft.source.inspect.v1", JobID: ids.FormatJobID(jobID), Attempt: attempt.AttemptNumber, Verdict: db.SourceVerificationLegacyUnverifiable}
	if source == nil {
		return view
	}
	if source.Pin != nil {
		view.Submitted = &sourceIdentityView{Kind: db.SourceIdentityManifestV2, SHA256: source.Pin.Hash}
		for _, root := range source.Pin.Roots {
			view.Roots = append(view.Roots, sourceRootView{MountRel: root.MountRel, MountBasename: root.MountBasename, SHA256: root.Hash, R2Key: root.R2Key, SizeBytes: root.SizeBytes, BlobCount: len(root.Blobs)})
		}
	}
	if source.Execution == nil {
		if view.Submitted != nil && !db.IsTerminalStatus(attempt.Status) {
			view.Verdict = db.SourceVerificationPending
		}
		return view
	}
	execution := *source.Execution
	view.Execution = &execution
	switch execution.Verification {
	case db.SourceVerificationMismatch, db.SourceVerificationObjectsUnavailable:
		view.Verdict = execution.Verification
	case db.SourceVerificationVerified:
		if view.Submitted != nil && execution.IdentityKind == view.Submitted.Kind && execution.DispatchedSHA256 == view.Submitted.SHA256 && execution.VerifiedSHA256 == view.Submitted.SHA256 {
			view.Verdict = db.SourceVerificationVerified
		}
	case db.SourceVerificationPending:
		view.Verdict = db.SourceVerificationPending
	}
	return view
}

func listPinnedSource(ctx context.Context, store sourceObjectStore, pin *db.JobSourcePinMetadata, prefix string, out io.Writer) error {
	prefix = cleanSourcePath(prefix)
	seen := make(map[string]struct{})
	var names []string
	for _, root := range pin.Roots {
		body, err := store.GetObjectReader(ctx, root.R2Key)
		if err != nil {
			return fmt.Errorf("fetch pinned source root from R2 key %s: %w", root.R2Key, err)
		}
		err = readSourceTarball(body, func(hdr *tar.Header, tr *tar.Reader) error {
			name := manifestSourcePath(root.MountRel, hdr.Name)
			if hdr.Typeflag == tar.TypeReg {
				_, _ = io.Copy(io.Discard, tr)
			}
			if sourcePathMatchesPrefix(name, prefix) {
				seen[name] = struct{}{}
			}
			return nil
		})
		closeErr := body.Close()
		if err != nil {
			return fmt.Errorf("list pinned source root %s: %w", root.R2Key, err)
		}
		if closeErr != nil {
			return closeErr
		}
		for _, blob := range root.Blobs {
			name := manifestSourcePath(root.MountRel, blob.RelPath)
			if sourcePathMatchesPrefix(name, prefix) {
				seen[name] = struct{}{}
			}
		}
	}
	for name := range seen {
		if name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintln(out, name)
	}
	return nil
}

func manifestSourcePath(mountRel, name string) string {
	name = cleanSourcePath(name)
	if mountRel == "" || mountRel == "." {
		return name
	}
	return path.Join(filepath.ToSlash(mountRel), name)
}

func sourcePathMatchesPrefix(name, prefix string) bool {
	return name != "" && (prefix == "" || name == prefix || strings.HasPrefix(name, prefix+"/"))
}

func runSourceCat(cmd *cobra.Command, args []string) error {
	jobID, err := parseSingleJobID(args[0])
	if err != nil {
		return err
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	client, err := newSourceStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sourceCmdTimeout)
	defer cancel()

	source, err := resolveJobSource(ctx, database, client, jobID, sourceAttempt)
	if err != nil {
		return err
	}

	targetPath := ""
	if len(args) == 2 {
		targetPath = args[1]
	} else {
		candidates := inferCommandSourcePaths(source.Job.Command)
		switch len(candidates) {
		case 0:
			return fmt.Errorf("could not infer source file from command %q; pass an explicit path", source.Job.Command)
		case 1:
			targetPath = candidates[0]
		default:
			return fmt.Errorf("command references multiple source files (%s); pass an explicit path", strings.Join(candidates, ", "))
		}
	}

	body, err := client.GetObjectReader(ctx, source.Mapping.R2Key)
	if err != nil {
		return fmt.Errorf("fetch source tarball from R2 key %s: %w", source.Mapping.R2Key, err)
	}
	defer body.Close()
	return catSourceTarballFile(body, targetPath, cmd.OutOrStdout())
}

func runSourceDiff(cmd *cobra.Command, args []string) error {
	aID, err := parseSingleJobID(args[0])
	if err != nil {
		return err
	}
	bID, err := parseSingleJobID(args[1])
	if err != nil {
		return err
	}
	target := ""
	if len(args) == 3 {
		target = cleanSourcePath(args[2])
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	client, err := newSourceStore()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sourceCmdTimeout)
	defer cancel()

	a, err := resolveJobSource(ctx, database, client, aID, sourceAttemptA)
	if err != nil {
		return err
	}
	b, err := resolveJobSource(ctx, database, client, bID, sourceAttemptB)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "weft-source-diff-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	aDir := filepath.Join(tmp, "a")
	bDir := filepath.Join(tmp, "b")
	if err := os.MkdirAll(aDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(bDir, 0o755); err != nil {
		return err
	}
	aLabel := ids.FormatJobID(a.Job.ID)
	bLabel := ids.FormatJobID(b.Job.ID)
	if err := extractSourceSnapshot(ctx, client, a.Mapping.R2Key, aDir); err != nil {
		return fmt.Errorf("extract %s source %s: %w", aLabel, a.Mapping.R2Key, err)
	}
	if err := extractSourceSnapshot(ctx, client, b.Mapping.R2Key, bDir); err != nil {
		return fmt.Errorf("extract %s source %s: %w", bLabel, b.Mapping.R2Key, err)
	}

	left := aDir
	right := bDir
	if target != "" {
		left = filepath.Join(aDir, filepath.FromSlash(target))
		right = filepath.Join(bDir, filepath.FromSlash(target))
	}
	return runRecursiveDiff(cmd.OutOrStdout(), left, right, aLabel, bLabel, target)
}

func parseSingleJobID(raw string) (int64, error) {
	jobIDs, err := ParseJobIDs([]string{raw})
	if err != nil {
		return 0, err
	}
	if len(jobIDs) != 1 {
		return 0, fmt.Errorf("expected one job ID, got %d", len(jobIDs))
	}
	return jobIDs[0], nil
}

func resolveJobSource(ctx context.Context, database *sql.DB, store sourceObjectStore, jobID int64, attemptNumber int) (*resolvedJobSource, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return nil, fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
	}
	if job == nil {
		return nil, fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}

	attempt, err := selectSourceAttempt(database, jobID, attemptNumber)
	if err != nil {
		return nil, err
	}
	if attempt.LaunchID == nil {
		return nil, fmt.Errorf("job %s attempt #%d has no source tarball; source inspection is only available for cloud attempts", ids.FormatJobID(jobID), attempt.AttemptNumber)
	}

	launchID := *attempt.LaunchID
	bootstrapKey := r2keys.BootstrapScript(launchID)
	bootstrapData, err := store.GetObject(ctx, bootstrapKey)
	if err != nil {
		return nil, fmt.Errorf("fetch bootstrap script from R2 key %s: %w", bootstrapKey, err)
	}
	sources, err := parseBootstrapSources(string(bootstrapData))
	if err != nil {
		return nil, fmt.Errorf("parse bootstrap script %s: %w", bootstrapKey, err)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("bootstrap script %s contains no source tarball mappings", bootstrapKey)
	}

	agentJob, _ := fetchCampaignAgentJob(ctx, store, launchID, jobID, attempt.ID)
	if agentJob != nil {
		if mapping, ok := sourceForRemoteDir(sources, agentJob.Dir); ok {
			return &resolvedJobSource{Job: job, Attempt: attempt, Mapping: mapping, Manifest: agentJob}, nil
		}
	}

	if len(sources) == 1 {
		return &resolvedJobSource{Job: job, Attempt: attempt, Mapping: sources[0], Manifest: agentJob}, nil
	}

	if agentJob == nil {
		return nil, fmt.Errorf("could not match job %s attempt #%d to one of %d source tarballs; campaign manifest %s did not contain the job", ids.FormatJobID(jobID), attempt.AttemptNumber, len(sources), r2keys.CampaignManifest(launchID))
	}
	return nil, fmt.Errorf("could not match job directory %q to one of %d source tarballs in bootstrap %s", agentJob.Dir, len(sources), bootstrapKey)
}

func selectSourceAttempt(database *sql.DB, jobID int64, attemptNumber int) (db.JobAttempt, error) {
	attempts, err := db.ListAttempts(database, jobID)
	if err != nil {
		return db.JobAttempt{}, err
	}
	if len(attempts) == 0 {
		return db.JobAttempt{}, fmt.Errorf("job %s has no attempts", ids.FormatJobID(jobID))
	}
	if attemptNumber == 0 {
		return attempts[0], nil
	}
	for _, attempt := range attempts {
		if attempt.AttemptNumber == attemptNumber {
			return attempt, nil
		}
	}
	return db.JobAttempt{}, fmt.Errorf("job %s has no attempt #%d", ids.FormatJobID(jobID), attemptNumber)
}

func fetchCampaignAgentJob(ctx context.Context, store sourceObjectStore, launchID, jobID, attemptID int64) (*cloud.AgentJob, error) {
	manifestKey := r2keys.CampaignManifest(launchID)
	data, err := store.GetObject(ctx, manifestKey)
	if err != nil {
		return nil, err
	}
	var manifest cloud.CampaignManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse campaign manifest %s: %w", manifestKey, err)
	}

	var fallback *cloud.AgentJob
	for i := range manifest.Jobs {
		job := &manifest.Jobs[i]
		if job.ID != jobID {
			continue
		}
		if job.RunID == attemptID {
			return job, nil
		}
		if fallback == nil {
			fallback = job
		}
	}
	return fallback, nil
}

var sourceCopyRe = regexp.MustCompile(`rclone copyto "r2:\$R2_BUCKET/([^"]+)" /tmp/src\.tar\.gz && tar xzf /tmp/src\.tar\.gz -C ("(?:\\.|[^"])*")`)

func parseBootstrapSources(script string) ([]sourceMapping, error) {
	var sources []sourceMapping
	for _, match := range sourceCopyRe.FindAllStringSubmatch(script, -1) {
		remoteDir, err := strconv.Unquote(match[2])
		if err != nil {
			return nil, fmt.Errorf("unquote remote source dir %s: %w", match[2], err)
		}
		sources = append(sources, sourceMapping{
			R2Key:     match[1],
			RemoteDir: remoteDir,
		})
	}
	return sources, nil
}

func sourceForRemoteDir(sources []sourceMapping, remoteDir string) (sourceMapping, bool) {
	remoteDir = cleanSourcePath(remoteDir)
	for _, source := range sources {
		if cleanSourcePath(source.RemoteDir) == remoteDir {
			return source, true
		}
	}
	return sourceMapping{}, false
}

func listSourceTarball(r io.Reader, prefix string, out io.Writer) error {
	prefix = cleanSourcePath(prefix)
	return readSourceTarball(r, func(hdr *tar.Header, tr *tar.Reader) error {
		name := cleanSourcePath(hdr.Name)
		if prefix != "" && name != prefix && !strings.HasPrefix(name, prefix+"/") {
			if hdr.Typeflag == tar.TypeReg {
				_, _ = io.Copy(io.Discard, tr)
			}
			return nil
		}
		if name != "" {
			fmt.Fprintln(out, name)
		}
		return nil
	})
}

func catSourceTarballFile(r io.Reader, target string, out io.Writer) error {
	target = cleanSourcePath(target)
	if target == "" {
		return fmt.Errorf("source path is required")
	}

	var foundNonRegular bool
	var found bool
	err := readSourceTarball(r, func(hdr *tar.Header, tr *tar.Reader) error {
		name := cleanSourcePath(hdr.Name)
		if name != target {
			if hdr.Typeflag == tar.TypeReg {
				_, _ = io.Copy(io.Discard, tr)
			}
			return nil
		}
		found = true
		if hdr.Typeflag != tar.TypeReg {
			foundNonRegular = true
			return nil
		}
		_, err := io.Copy(out, tr)
		return err
	})
	if err != nil {
		return err
	}
	if foundNonRegular {
		return fmt.Errorf("source path %q is not a regular file", target)
	}
	if !found {
		return fmt.Errorf("source path %q not found", target)
	}
	return nil
}

func readSourceTarball(r io.Reader, visit func(*tar.Header, *tar.Reader) error) error {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}
		if err := visit(hdr, tr); err != nil {
			return err
		}
	}
}

func extractSourceSnapshot(ctx context.Context, store sourceObjectStore, key, dir string) error {
	body, err := store.GetObjectReader(ctx, key)
	if err != nil {
		return fmt.Errorf("fetch source tarball from R2 key %s: %w", key, err)
	}
	defer body.Close()
	return srcsync.ExtractTarballReader(body, dir)
}

func runRecursiveDiff(out io.Writer, left, right, leftLabel, rightLabel, target string) error {
	args := []string{"-ruN", left, right}
	cmd := exec.Command("diff", args...)
	data, err := cmd.CombinedOutput()
	text := string(data)
	if target != "" {
		leftLabel += "/" + target
		rightLabel += "/" + target
	}
	text = strings.ReplaceAll(text, left, leftLabel)
	text = strings.ReplaceAll(text, right, rightLabel)
	if text != "" {
		fmt.Fprint(out, text)
	}
	if err == nil {
		fmt.Fprintln(out, "No source differences.")
		return nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return nil
	}
	return fmt.Errorf("run diff: %w", err)
}

var commandSourceExts = []string{".py", ".sh", ".bash", ".R", ".jl"}

func inferCommandSourcePaths(command string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, token := range strings.Fields(command) {
		candidate := strings.Trim(token, `"'`)
		candidate = strings.TrimRight(candidate, `;:,`)
		if candidate == "" || strings.HasPrefix(candidate, "-") || strings.Contains(candidate, "=") {
			continue
		}
		ext := path.Ext(candidate)
		if !slices.Contains(commandSourceExts, ext) {
			continue
		}
		candidate = cleanSourcePath(candidate)
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func cleanSourcePath(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "./")
	raw = strings.TrimPrefix(raw, "/")
	if raw == "" || raw == "." {
		return ""
	}
	return path.Clean(raw)
}
