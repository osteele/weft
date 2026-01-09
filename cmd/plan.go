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

	"github.com/osteele/remote-jobs/internal/core"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/plan"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Submit and manage job execution plans",
}

var planSubmitCmd = &cobra.Command{
	Use:   "submit <file|- >",
	Short: "Submit a YAML job execution plan",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runPlanSubmit,
}

var planValidateCmd = &cobra.Command{
	Use:   "validate <file|- >",
	Short: "Validate a YAML job execution plan without submitting",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runPlanValidate,
}

var planShowCmd = &cobra.Command{
	Use:   "show <file|- >",
	Short: "Inspect plan jobs, IDs, and dependencies",
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runPlanShow,
}

var (
	planWatchDuration time.Duration
	planNoQueueStart  bool
	planDefaultHost   string
	planValidateHost  string
	planShowHost      string
	planShowIDsOnly   bool
)

func init() {
	rootCmd.AddCommand(planCmd)
	planCmd.AddCommand(planSubmitCmd)
	planCmd.AddCommand(planValidateCmd)
	planCmd.AddCommand(planShowCmd)
	planSubmitCmd.Flags().DurationVar(&planWatchDuration, "watch", 0, "Wait for up to this duration and report job outcomes")
	planSubmitCmd.Flags().BoolVar(&planNoQueueStart, "no-queue-start", false, "Skip auto-starting queue runners for queued jobs")
	planSubmitCmd.Flags().StringVarP(&planDefaultHost, "host", "H", "", "Default host for jobs that omit the host field")
	planValidateCmd.Flags().StringVarP(&planValidateHost, "host", "H", "", "Default host for jobs that omit the host field")
	planShowCmd.Flags().StringVarP(&planShowHost, "host", "H", "", "Default host for jobs that omit the host field")
	planShowCmd.Flags().BoolVar(&planShowIDsOnly, "ids", false, "Only print IDs and aliases without job details")
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
	planFile, err := loadPlanFile(path, planDefaultHost)
	if err != nil {
		return err
	}
	execPlan, err := planFile.BuildExecutionPlan()
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	coreSvc := core.NewServiceWithDB(database)

	if len(planFile.Kill) > 0 {
		for _, id := range planFile.Kill {
			oplog.Log(oplog.OpCLICommand, oplog.WithDetail("plan kill"), oplog.WithJobID(id))
			result, err := killJobWithService(coreSvc, id, ops.TimeoutNormal)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to kill job %d: %v\n", id, err)
			} else {
				message := result.Outcome.Message
				if message == "" {
					message = fmt.Sprintf("Killed job %d", id)
				}
				fmt.Println(message)
			}
		}
		fmt.Println()
	}

	var scheduled []scheduledPlanJob
	commandMap := make(map[string][]int64)
	startedQueues := make(map[string]bool)

	scheduled, err = scheduleExecutionPlan(database, execPlan, startedQueues)
	if err != nil {
		return err
	}
	for _, sj := range scheduled {
		commandMap[sj.Command] = append(commandMap[sj.Command], sj.JobID)
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

func loadPlanFile(path, defaultHost string) (*plan.File, error) {
	data, err := readPlanInput(path)
	if err != nil {
		return nil, err
	}
	planFile, err := plan.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("parse plan: %w", err)
	}
	if err := planFile.ApplyDefaults(plan.Defaults{Host: defaultHost}); err != nil {
		return nil, err
	}
	if err := planFile.Validate(); err != nil {
		return nil, err
	}
	return planFile, nil
}

func scheduleExecutionPlan(database *sql.DB, execPlan *plan.ExecutionPlan, startedQueues map[string]bool) ([]scheduledPlanJob, error) {
	idToJob := make(map[string]int64)
	var scheduled []scheduledPlanJob

	for _, job := range execPlan.Jobs {
		blockDir := ""
		var blockEnv map[string]string
		blockQueue := ""
		if job.Block != nil {
			blockDir = job.Block.Dir
			blockEnv = job.Block.Env
			blockQueue = job.Block.Queue
		}
		resolved := applyJobDefaults(*job.Source, blockDir, blockEnv)
		queueRequired := job.Source.QueueOnly || (job.Block != nil && job.Block.Kind == plan.BlockKindSeries) || len(job.Dependencies) > 0
		queueName := job.Source.Queue
		if queueName == "" {
			queueName = blockQueue
		}
		label := jobLabel(resolved)

		if queueRequired {
			deps := make([]queueDependency, 0, len(job.Dependencies))
			for _, dep := range job.Dependencies {
				depJobID, ok := idToJob[dep.Job.ID]
				if !ok {
					return nil, fmt.Errorf("%s: dependency %s has not been scheduled yet", job.Path, dep.Job.ID)
				}
				deps = append(deps, queueDependency{JobID: depJobID, AllowFailure: dep.Optional})
			}
			targetQueue := queueName
			if targetQueue == "" {
				targetQueue = defaultQueueName
			}
			res, err := queueJob(database, queueJobOptions{
				Host:         resolved.Host,
				WorkingDir:   resolved.Dir,
				Command:      resolved.Command,
				Description:  resolved.Description,
				EnvVars:      resolved.EnvVars,
				QueueName:    targetQueue,
				Dependencies: deps,
				AutoStart:    !planNoQueueStart,
			})
			if err != nil {
				return nil, err
			}
			jobID := res.JobID
			idToJob[job.ID] = jobID
			if res.Deferred {
				fmt.Printf("Job %s recorded as %d for queue %s on %s (host unreachable, will sync later)\n", label, jobID, targetQueue, resolved.Host)
			} else {
				fmt.Printf("Job %s queued as %d on %s (queue %s)\n", label, jobID, resolved.Host, targetQueue)
				maybeStartQueueRunner(resolved.Host, targetQueue, startedQueues)
			}
			scheduled = append(scheduled, scheduledPlanJob{
				Label:     label,
				Command:   resolved.Command,
				Host:      resolved.Host,
				QueueName: targetQueue,
				JobID:     jobID,
			})
			continue
		}

		result, err := startJob(database, startJobOptions{
			Host:        resolved.Host,
			WorkingDir:  resolved.Dir,
			Command:     resolved.Command,
			Description: resolved.Description,
			EnvVars:     resolved.EnvVars,
		})
		if err != nil {
			return nil, err
		}
		idToJob[job.ID] = result.Info.JobID
		if result.DeferredToQueue {
			fmt.Printf("SSH to %s failed; job %d will be added to the remote queue on next sync\n", resolved.Host, result.Info.JobID)
		}
		scheduled = append(scheduled, scheduledPlanJob{
			Label:   label,
			Command: resolved.Command,
			Host:    resolved.Host,
			JobID:   result.Info.JobID,
		})
	}

	return scheduled, nil
}

func runPlanValidate(cmd *cobra.Command, args []string) error {
	path := args[0]
	planFile, err := loadPlanFile(path, planValidateHost)
	if err != nil {
		return err
	}
	execPlan, err := planFile.BuildExecutionPlan()
	if err != nil {
		return err
	}
	fmt.Printf("Plan %s validated successfully (%d jobs)\n", path, len(execPlan.Jobs))
	return nil
}

func runPlanShow(cmd *cobra.Command, args []string) error {
	path := args[0]
	planFile, err := loadPlanFile(path, planShowHost)
	if err != nil {
		return err
	}
	execPlan, err := planFile.BuildExecutionPlan()
	if err != nil {
		return err
	}

	if planShowIDsOnly {
		fmt.Println("Plan identifiers:")
		for _, job := range execPlan.Jobs {
			if job.Alias != "" {
				fmt.Printf("  %s (alias %s)\n", job.ID, job.Alias)
			} else {
				fmt.Printf("  %s\n", job.ID)
			}
		}
		return nil
	}

	fmt.Println("Plan jobs:")
	for _, job := range execPlan.Jobs {
		fmt.Printf("- %s", job.ID)
		if job.Alias != "" {
			fmt.Printf(" (alias %s)", job.Alias)
		}
		if job.Block != nil && job.Block.ID != "" {
			fmt.Printf(" [block %s]", job.Block.ID)
		}
		fmt.Printf("\n    host: %s\n", job.Source.Host)
		if job.Source.Dir != "" || (job.Block != nil && job.Block.Dir != "") {
			if job.Source.Dir != "" {
				fmt.Printf("    dir: %s\n", job.Source.Dir)
			} else if job.Block != nil && job.Block.Dir != "" {
				fmt.Printf("    dir: %s (from block)\n", job.Block.Dir)
			}
		}
		fmt.Printf("    cmd: %s\n", job.Source.Command)
		if len(job.Dependencies) > 0 {
			fmt.Printf("    depends_on:\n")
			for _, dep := range job.Dependencies {
				mode := "success"
				if dep.Optional {
					mode = "any"
				}
				fmt.Printf("      - %s (%s)\n", dep.Job.ID, mode)
			}
		}
	}
	return nil
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
		res, err := queueJob(database, queueJobOptions{
			Host:        job.Host,
			WorkingDir:  job.Dir,
			Command:     job.Command,
			Description: job.Description,
			EnvVars:     job.EnvVars,
			QueueName:   queueName,
			AutoStart:   !planNoQueueStart,
		})
		if err != nil {
			return scheduledPlanJob{}, err
		}
		jobID := res.JobID
		if res.Deferred {
			fmt.Printf("Job %s recorded as %d for queue %s on %s (host unreachable, will sync later)\n", label, jobID, queueName, job.Host)
		} else {
			fmt.Printf("Job %s queued as %d on %s (queue %s)\n", label, jobID, job.Host, queueName)
			maybeStartQueueRunner(job.Host, queueName, startedQueues)
		}
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
	if result.DeferredToQueue {
		fmt.Printf("SSH to %s failed; job %d will be queued remotely on next sync\n", job.Host, result.Info.JobID)
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
	if !usageHintsEnabled() {
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
	tracker := newHostConnectionTracker()

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
				if ssh.IsConnectionError(err.Error()) {
					tracker.MarkDown(host)
					continue
				}
				fmt.Fprintf(os.Stderr, "Warning: sync %s: %v\n", host, err)
				continue
			}
			pending, err := hostHasPendingPlanJobs(database, host, jobs)
			if err != nil {
				return err
			}
			tracker.MarkUp(host, pending)
		}
		time.Sleep(3 * time.Second)
	}

	printWatchSummary(statusByID, jobs)
	return nil
}

func jobTerminal(job *db.Job) bool {
	switch job.Status {
	case db.StatusCompleted, db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
		return true
	default:
		return false
	}
}

func hostHasPendingPlanJobs(database *sql.DB, host string, jobs []scheduledPlanJob) (bool, error) {
	for _, job := range jobs {
		if job.Host != host {
			continue
		}
		record, err := db.GetJobByID(database, job.JobID)
		if err != nil {
			return false, err
		}
		if record != nil && !jobTerminal(record) {
			return true, nil
		}
	}
	return false, nil
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
	case db.StatusKilled:
		return "killed"
	case db.StatusCanceled:
		return "canceled"
	case db.StatusQueued:
		return "queued"
	case db.StatusRunning, db.StatusStarting:
		return "running"
	default:
		return job.Status
	}
}
