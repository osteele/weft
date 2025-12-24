package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/plan"
	"github.com/spf13/cobra"
)

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Submit and manage job execution plans",
}

var planSubmitCmd = &cobra.Command{
	Use:   "submit <file|- >",
	Short: "Submit a YAML job execution plan",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlanSubmit,
}

var (
	planWatchDuration time.Duration
	planNoQueueStart  bool
	planDefaultHost   string
)

func init() {
	rootCmd.AddCommand(planCmd)
	planCmd.AddCommand(planSubmitCmd)
	planSubmitCmd.Flags().DurationVar(&planWatchDuration, "watch", 0, "Wait for up to this duration and report job outcomes")
	planSubmitCmd.Flags().BoolVar(&planNoQueueStart, "no-queue-start", false, "Skip auto-starting queue runners for queued jobs")
	planSubmitCmd.Flags().StringVarP(&planDefaultHost, "host", "H", "", "Default host for jobs that omit the host field")
}

type scheduledPlanJob struct {
	Label     string
	Command   string
	Host      string
	QueueName string
	JobID     int64
}

func runPlanSubmit(cmd *cobra.Command, args []string) error {
	path := args[0]
	data, err := readPlanInput(path)
	if err != nil {
		return err
	}

	planFile, err := plan.Decode(data)
	if err != nil {
		return fmt.Errorf("parse plan: %w", err)
	}
	if err := planFile.ApplyDefaults(plan.Defaults{Host: planDefaultHost}); err != nil {
		return err
	}
	if err := planFile.Validate(); err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if len(planFile.Kill) > 0 {
		for _, id := range planFile.Kill {
			if err := killJob(database, id); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to kill job %d: %v\n", id, err)
			} else {
				fmt.Printf("Killed job %d\n", id)
			}
		}
		fmt.Println()
	}

	var scheduled []scheduledPlanJob
	commandMap := make(map[string][]int64)
	startedQueues := make(map[string]bool)

	for idx, entry := range planFile.Jobs {
		label := fmt.Sprintf("jobs[%d]", idx)
		subJobs, err := schedulePlanEntry(database, entry, startedQueues)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		for _, sj := range subJobs {
			scheduled = append(scheduled, sj)
			commandMap[sj.Command] = append(commandMap[sj.Command], sj.JobID)
		}
	}

	printCommandMap(commandMap)
	printPlanStatusCommands(scheduled)

	if planWatchDuration > 0 {
		if err := watchPlanJobs(database, scheduled, planWatchDuration); err != nil {
			return err
		}
	}

	return nil
}

func readPlanInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func schedulePlanEntry(database *sql.DB, entry plan.Entry, startedQueues map[string]bool) ([]scheduledPlanJob, error) {
	switch {
	case entry.Job != nil:
		job := applyJobDefaults(*entry.Job, "", nil)
		sj, err := scheduleSingleJob(database, job, startedQueues)
		if err != nil {
			return nil, err
		}
		return []scheduledPlanJob{sj}, nil
	case entry.Parallel != nil:
		return scheduleParallelBlock(database, entry.Parallel, startedQueues)
	case entry.Series != nil:
		return scheduleSeriesBlock(database, entry.Series, startedQueues)
	default:
		return nil, fmt.Errorf("invalid plan entry")
	}
}

func scheduleParallelBlock(database *sql.DB, block *plan.Parallel, startedQueues map[string]bool) ([]scheduledPlanJob, error) {
	var out []scheduledPlanJob
	for _, job := range block.Jobs {
		resolved := applyJobDefaults(job, block.Dir, block.Env)
		sj, err := scheduleSingleJob(database, resolved, startedQueues)
		if err != nil {
			return nil, err
		}
		out = append(out, sj)
	}
	return out, nil
}

func scheduleSeriesBlock(database *sql.DB, block *plan.Series, startedQueues map[string]bool) ([]scheduledPlanJob, error) {
	queueName := block.Queue
	if queueName == "" {
		queueName = defaultQueueName
	}
	var out []scheduledPlanJob
	var prevJobID int64
	waitMode := block.Wait
	if waitMode == "" {
		waitMode = "success"
	}
	detectedHost := ""
	for i, job := range block.Jobs {
		resolved := applyJobDefaults(job, block.Dir, block.Env)
		if detectedHost == "" {
			detectedHost = resolved.Host
		} else if resolved.Host != detectedHost {
			return nil, fmt.Errorf("series block jobs must target the same host (found %s and %s)", detectedHost, resolved.Host)
		}
		afterID := int64(0)
		afterAny := false
		if i > 0 {
			afterID = prevJobID
			afterAny = waitMode == "any"
		}
		jobID, err := queueJob(database, queueJobOptions{
			Host:        resolved.Host,
			WorkingDir:  resolved.Dir,
			Command:     resolved.Command,
			Description: resolved.Description,
			EnvVars:     resolved.EnvVars,
			QueueName:   queueName,
			AfterJobID:  afterID,
			AfterAny:    afterAny,
		})
		if err != nil {
			return nil, err
		}
		prevJobID = jobID
		out = append(out, scheduledPlanJob{
			Label:     jobLabel(resolved),
			Command:   resolved.Command,
			Host:      resolved.Host,
			QueueName: queueName,
			JobID:     jobID,
		})
		fmt.Printf("Series job %s queued as %d on %s (queue %s)\n", jobLabel(resolved), jobID, resolved.Host, queueName)
		maybeStartQueueRunner(resolved.Host, queueName, startedQueues)
	}
	return out, nil
}

type resolvedPlanJob struct {
	plan.Job
	Dir     string
	EnvVars []string
}

func applyJobDefaults(job plan.Job, defaultDir string, defaultEnv map[string]string) resolvedPlanJob {
	mergedDir := job.Dir
	if mergedDir == "" {
		mergedDir = defaultDir
	}
	mergedEnv := map[string]string{}
	for k, v := range defaultEnv {
		mergedEnv[k] = v
	}
	for k, v := range job.Env {
		mergedEnv[k] = v
	}
	return resolvedPlanJob{
		Job:     job,
		Dir:     mergedDir,
		EnvVars: applyEnvMap(mergedEnv),
	}
}

func scheduleSingleJob(database *sql.DB, job resolvedPlanJob, startedQueues map[string]bool) (scheduledPlanJob, error) {
	label := jobLabel(job)
	if job.QueueOnly {
		queueName := job.Queue
		if queueName == "" {
			queueName = defaultQueueName
		}
		jobID, err := queueJob(database, queueJobOptions{
			Host:        job.Host,
			WorkingDir:  job.Dir,
			Command:     job.Command,
			Description: job.Description,
			EnvVars:     job.EnvVars,
			QueueName:   queueName,
		})
		if err != nil {
			return scheduledPlanJob{}, err
		}
		fmt.Printf("Job %s queued as %d on %s (queue %s)\n", label, jobID, job.Host, queueName)
		maybeStartQueueRunner(job.Host, queueName, startedQueues)
		return scheduledPlanJob{Label: label, Command: job.Command, Host: job.Host, QueueName: queueName, JobID: jobID}, nil
	}

	result, err := startJob(database, startJobOptions{
		Host:        job.Host,
		WorkingDir:  job.Dir,
		Command:     job.Command,
		Description: job.Description,
		EnvVars:     job.EnvVars,
		OnPrepared: func(info StartJobPreparedInfo) {
			fmt.Printf("Starting %s as job %d on %s\n", label, info.JobID, job.Host)
		},
	})
	if err != nil {
		return scheduledPlanJob{}, err
	}
	if result.QueuedOnConnectionFailure {
		fmt.Printf("Connection to %s failed; job %d queued locally for retry\n", job.Host, result.Info.JobID)
		return scheduledPlanJob{Label: label, Command: job.Command, Host: job.Host, JobID: result.Info.JobID}, nil
	}
	fmt.Printf("Job %s started as %d on %s\n", label, result.Info.JobID, job.Host)
	return scheduledPlanJob{Label: label, Command: job.Command, Host: job.Host, JobID: result.Info.JobID}, nil
}

func jobLabel(job resolvedPlanJob) string {
	if job.Name != "" {
		return job.Name
	}
	return job.Command
}

func maybeStartQueueRunner(host, queue string, started map[string]bool) {
	if planNoQueueStart {
		return
	}
	key := fmt.Sprintf("%s|%s", host, queue)
	if started[key] {
		return
	}
	started[key] = true
	startedRunner, err := ensureQueueRunnerStarted(host, queue)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to start queue runner on %s (%s): %v\n", host, queue, err)
		return
	}
	if startedRunner {
		fmt.Printf("Queue runner started on %s (%s)\n", host, queue)
	}
}

func printCommandMap(m map[string][]int64) {
	fmt.Println()
	fmt.Println("Command to job IDs:")
	commands := make([]string, 0, len(m))
	for cmd := range m {
		commands = append(commands, cmd)
	}
	sort.Strings(commands)
	for _, command := range commands {
		ids := m[command]
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if len(ids) == 1 {
			fmt.Printf("  %s: %d\n", command, ids[0])
			continue
		}
		fmt.Printf("  %s:\n", command)
		for _, id := range ids {
			fmt.Printf("    - %d\n", id)
		}
	}
}

func printPlanStatusCommands(jobs []scheduledPlanJob) {
	if len(jobs) == 0 {
		return
	}
	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		if job.JobID > 0 {
			ids = append(ids, fmt.Sprintf("%d", job.JobID))
		}
	}
	if len(ids) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("Monitor plan progress:")
	fmt.Printf("  remote-jobs status %s\n", strings.Join(ids, " "))
	fmt.Printf("  remote-jobs status --wait %s\n", strings.Join(ids, " "))
	fmt.Printf("  remote-jobs status --wait --wait-timeout 30m %s\n", strings.Join(ids, " "))
}

func watchPlanJobs(database *sql.DB, jobs []scheduledPlanJob, duration time.Duration) error {
	if len(jobs) == 0 {
		fmt.Println("No jobs scheduled; nothing to watch.")
		return nil
	}
	deadline := time.Now().Add(duration)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	statusByID := make(map[int64]*db.Job)

	for {
		completed := true
		hostsToSync := map[string]struct{}{}
		for _, job := range jobs {
			record, err := db.GetJobByID(database, job.JobID)
			if err != nil {
				return err
			}
			if record != nil {
				statusByID[job.JobID] = record
				if !jobTerminal(record) {
					completed = false
					hostsToSync[job.Host] = struct{}{}
				}
			}
		}
		if completed {
			break
		}
		if ctx.Err() != nil {
			break
		}
		for host := range hostsToSync {
			if _, err := syncHost(database, host); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: sync %s: %v\n", host, err)
			}
		}
		time.Sleep(3 * time.Second)
	}

	printWatchSummary(statusByID, jobs)
	return nil
}

func jobTerminal(job *db.Job) bool {
	switch job.Status {
	case db.StatusCompleted, db.StatusDead, db.StatusFailed:
		return true
	default:
		return false
	}
}

func printWatchSummary(statusByID map[int64]*db.Job, jobs []scheduledPlanJob) {
	fmt.Println()
	fmt.Println("Watch summary:")
	for _, job := range jobs {
		record := statusByID[job.JobID]
		status := "unknown"
		if record != nil {
			status = classifyJobStatus(record)
		}
		fmt.Printf("  %s (job %d on %s): %s\n", job.Label, job.JobID, job.Host, status)
	}
}

func classifyJobStatus(job *db.Job) string {
	switch job.Status {
	case db.StatusCompleted:
		if job.ExitCode != nil && *job.ExitCode == 0 {
			return "succeeded"
		}
		return "failed"
	case db.StatusDead, db.StatusFailed:
		return "failed"
	case db.StatusQueued, db.StatusPending:
		return "queued"
	case db.StatusRunning, db.StatusStarting:
		return "running"
	default:
		return job.Status
	}
}
