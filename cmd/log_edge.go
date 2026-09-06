package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/edgeview"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/logfiles"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
)

func produceJobLogTailSection(ctx context.Context, deps edgeViewDeps, jobID int64) ([]byte, error) {
	job, err := db.GetJobByID(deps.DB, jobID)
	if err != nil {
		return nil, fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
	}
	if job == nil {
		return nil, fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	content, err := readJobLogForView(readCtx, deps.DB, deps.Cfg, job)
	if err != nil {
		return nil, err
	}
	return lastBytes([]byte(content), edgeview.LogTailBytes), nil
}

func readJobLogForView(ctx context.Context, database *sql.DB, cfg *config.Config, job *db.Job) (string, error) {
	if job.EffectiveStatus() == db.StatusQueued && job.StartTime == 0 {
		provenance, err := loadAttemptDisplayProvenance(database, job)
		if err != nil {
			return "", fmt.Errorf("list attempts for job %s: %w", ids.FormatJobID(job.ID), err)
		}
		if provenance.latestStarted == nil {
			return queuedJobLogNotice(database, job), nil
		}
		job = jobForAttempt(job, provenance.latestStarted)
	}
	if cached, err := logcache.Read(job.ID); err == nil && shouldPreferCachedLog(job.Status) && shouldUseCachedLogForJob(job) {
		return cached, nil
	}
	if shouldUseCloudLogs(database, job) || job.Backend == db.BackendSkyPilot {
		return readJobLogSnapshotForView(ctx, database, cfg, job)
	}
	logFile, resolved := logfiles.ResolveWithTimeout(job, FastSyncTimeout)
	if !resolved {
		exists, err := ssh.RemoteFileExistsWithTimeout(job.Host, logFile, FastSyncTimeout)
		if err != nil {
			if fallback, fallbackErr := readJobLogSnapshotForView(ctx, database, cfg, job); fallbackErr == nil {
				return fallback, nil
			}
			return "", fmt.Errorf("check log for job %s on %s: %w", ids.FormatJobID(job.ID), job.Host, err)
		}
		if !exists {
			return readJobLogSnapshotForView(ctx, database, cfg, job)
		}
	}
	command := fmt.Sprintf("tail -c %d -- %s", edgeview.LogTailBytes, shellQuote(logFile))
	stdout, stderr, err := ssh.RunWithTimeout(job.Host, command, FastSyncTimeout)
	if err == nil {
		return stdout, nil
	}
	if cached, cacheErr := logcache.Read(job.ID); cacheErr == nil && shouldUseCachedLogForJob(job) {
		return cached, nil
	}
	if fallback, fallbackErr := readJobLogSnapshotForView(ctx, database, cfg, job); fallbackErr == nil {
		return fallback, nil
	}
	if stderr != "" {
		return "", fmt.Errorf("read log for job %s on %s: %s", ids.FormatJobID(job.ID), job.Host, strings.TrimSpace(stderr))
	}
	return "", fmt.Errorf("read log for job %s on %s: %w", ids.FormatJobID(job.ID), job.Host, err)
}

func readJobLogSnapshotForView(ctx context.Context, database *sql.DB, cfg *config.Config, job *db.Job) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("log snapshot configuration unavailable for job %s", ids.FormatJobID(job.ID))
	}
	client, err := buildR2Client(cfg)
	if err != nil {
		return "", contextualizeCloudLogFetchError(database, job, fmt.Errorf("create log snapshot client: %w", err))
	}
	if client == nil {
		return "", contextualizeCloudLogFetchError(database, job, fmt.Errorf("log snapshot storage not configured"))
	}
	fetched, err := fetchCloudLogFromR2(ctx, client, job.ID, cloudLogRunID(job), 0, 0, 0)
	if err != nil {
		return "", contextualizeCloudLogFetchError(database, job, err)
	}
	return fetched.Content, nil
}

func lastBytes(data []byte, limit int) []byte {
	if limit <= 0 || len(data) <= limit {
		return data
	}
	return data[len(data)-limit:]
}

func runLogEdge(cmd *cobra.Command, args []string, em *edgeMirrorRuntime) error {
	if logOps || logEvents {
		return fmt.Errorf("operations and lifecycle-event logs are not in the hub view")
	}
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}
	if logFollow && len(jobIDs) != 1 {
		return fmt.Errorf("--follow can only be used with a single job ID")
	}
	if logLines < 0 {
		return fmt.Errorf("--lines/--tail must be nonnegative")
	}
	if logFull {
		if cmd.Flags().Changed("from") {
			return fmt.Errorf("--full cannot be combined with --from")
		}
		logFrom = 1
	}
	hasLineRange := logFrom > 0 || logTo > 0
	if hasLineRange && (cmd.Flags().Changed("lines") || cmd.Flags().Changed("tail")) {
		return fmt.Errorf("--from/--to cannot be used with -n/--lines/--tail")
	}
	if logFollow && logTo > 0 {
		return fmt.Errorf("--follow cannot be used with --to")
	}
	if logAttempt > 0 {
		return fmt.Errorf("--attempt needs archived logs outside the bounded hub view")
	}
	if logSync || logTimeout > 0 {
		return fmt.Errorf("log sync and SSH timeout flags need live host access; omit them to read the hub view")
	}
	for i, jobID := range jobIDs {
		if i > 0 {
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintf(cmd.OutOrStdout(), "Job %s:\n", ids.FormatJobID(jobID))
		}
		if err := renderJobLogFromView(cmd, em, jobID, logFollow); err != nil {
			return err
		}
	}
	return nil
}

func renderJobLogFromView(cmd *cobra.Command, em *edgeMirrorRuntime, jobID int64, follow bool) error {
	section := edgeview.JobLogTailSection(ids.FormatJobID(jobID))
	body, prov, err := em.fetch(cmd, section)
	if err != nil {
		return err
	}
	if err := validateEdgeLogRange(cmd, body); err != nil {
		return err
	}
	queuedNotice := isQueuedJobLogNotice(jobID, body)
	if shouldShowDefaultTailHint(cmd, follow) && !queuedNotice {
		printDefaultTailHint(cmd.OutOrStdout(), jobID)
	}
	if _, err := fmt.Fprint(cmd.OutOrStdout(), processCarriageReturns(filterLogContent(string(body), logFrom, logTo, logLines, logGrep))); err != nil {
		return err
	}
	printEdgeProvenance(cmd.OutOrStdout(), prov)
	if !follow {
		return nil
	}

	previous := body
	interval := em.publishInterval
	if interval <= 0 {
		interval = config.DefaultEdgeViewPublishInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-cmd.Context().Done():
			return nil
		case <-ticker.C:
			next, nextProv, err := em.fetch(cmd, section)
			if err != nil {
				return err
			}
			delta := logTailDelta(previous, next)
			if len(delta) > 0 {
				deltaText := string(delta)
				if logGrep != "" {
					deltaText = filterLogContent(deltaText, 1, 0, 0, logGrep)
				}
				if _, err := fmt.Fprint(cmd.OutOrStdout(), processCarriageReturns(deltaText)); err != nil {
					return err
				}
				printEdgeProvenance(cmd.OutOrStdout(), nextProv)
			}
			previous = next
		}
	}
}

func queuedJobLogNotice(database *sql.DB, job *db.Job) string {
	var output bytes.Buffer
	fmt.Fprintf(&output, "Job %s has not started yet; no logs are available.\n", ids.FormatJobID(job.ID))
	printPlacementLines(&output, queuedPlacementLines(database, job), 12)
	fmt.Fprintf(&output, "Attempts:    weft info %s --all-attempts\n", ids.FormatJobID(job.ID))
	return output.String()
}

func isQueuedJobLogNotice(jobID int64, body []byte) bool {
	prefix := fmt.Sprintf("Job %s has not started yet; no logs are available.\n", ids.FormatJobID(jobID))
	return bytes.HasPrefix(body, []byte(prefix))
}

func validateEdgeLogRange(cmd *cobra.Command, body []byte) error {
	truncated := len(body) == edgeview.LogTailBytes
	if truncated && (logFull || logFrom > 0 || logTo > 0) {
		return fmt.Errorf("requested log range starts outside the hub view log tail bound of %d bytes", edgeview.LogTailBytes)
	}
	if truncated && (cmd.Flags().Changed("lines") || cmd.Flags().Changed("tail")) {
		completeLines := bytes.Count(body, []byte{'\n'})
		// The first line may begin before the byte window. When the window
		// ends on a newline, discount that possibly partial first line; when
		// it does not, the final unterminated line takes its place.
		if len(body) > 0 && body[len(body)-1] == '\n' && completeLines > 0 {
			completeLines--
		}
		if logLines > completeLines {
			return fmt.Errorf("requested --lines %d exceeds the hub view log tail bound of %d bytes (%d complete lines available)",
				logLines, edgeview.LogTailBytes, completeLines)
		}
	}
	return nil
}

func logTailDelta(previous, next []byte) []byte {
	if bytes.HasPrefix(next, previous) {
		return next[len(previous):]
	}
	max := len(previous)
	if len(next) < max {
		max = len(next)
	}
	overlap := longestSuffixPrefix(previous[len(previous)-max:], next[:max])
	return next[overlap:]
}

// longestSuffixPrefix returns the longest suffix of text that is also a
// prefix of pattern. KMP keeps a no-overlap 256 KiB tail comparison linear.
func longestSuffixPrefix(text, pattern []byte) int {
	if len(text) == 0 || len(pattern) == 0 {
		return 0
	}
	prefix := make([]int, len(pattern))
	for i, matched := 1, 0; i < len(pattern); i++ {
		for matched > 0 && pattern[i] != pattern[matched] {
			matched = prefix[matched-1]
		}
		if pattern[i] == pattern[matched] {
			matched++
		}
		prefix[i] = matched
	}
	matched := 0
	for i, b := range text {
		for matched > 0 && (matched == len(pattern) || b != pattern[matched]) {
			matched = prefix[matched-1]
		}
		if b == pattern[matched] {
			matched++
		}
		if matched == len(pattern) && i != len(text)-1 {
			matched = prefix[matched-1]
		}
	}
	return matched
}
