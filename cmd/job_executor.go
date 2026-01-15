package cmd

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/slack"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// startJobOptions controls how a job is started immediately on the remote host.
type startJobOptions struct {
	Host        string
	WorkingDir  string
	Command     string
	Description string
	EnvVars     []string
	Tags        []string
	Timeout     string
	OnPrepared  func(info StartJobPreparedInfo)
}

// StartJobPreparedInfo exposes metadata about the job once it has an ID.
type StartJobPreparedInfo struct {
	JobID        int64
	Host         string
	WorkingDir   string
	Command      string
	Description  string
	StartTime    int64
	TmuxSession  string
	LogFile      string
	StatusFile   string
	MetadataFile string
	PidFile      string
}

// startJobResult reports the outcome of the start operation.
type startJobResult struct {
	Info            StartJobPreparedInfo
	SlackEnabled    bool
	DeferredToQueue bool
}

func startJob(database *sql.DB, opts startJobOptions) (*startJobResult, error) {
	if opts.WorkingDir == "" {
		var err error
		opts.WorkingDir, err = session.DefaultWorkingDir()
		if err != nil {
			return nil, fmt.Errorf("get working dir: %w", err)
		}
	}

	jobID, err := db.RecordJobStarting(database, opts.Host, opts.WorkingDir, opts.Command, opts.Description)
	if err != nil {
		return nil, fmt.Errorf("create job record: %w", err)
	}
	if err := db.SetJobTags(database, jobID, opts.Tags); err != nil {
		return nil, fmt.Errorf("set job tags: %w", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		return nil, fmt.Errorf("get job: %w", err)
	}

	info := StartJobPreparedInfo{
		JobID:        jobID,
		Host:         job.Host,
		WorkingDir:   job.WorkingDir,
		Command:      job.Command,
		Description:  job.Description,
		StartTime:    job.StartTime,
		TmuxSession:  session.TmuxSessionName(jobID),
		LogFile:      session.SimpleLogFile(jobID),
		StatusFile:   session.SimpleStatusFile(jobID),
		MetadataFile: session.SimpleMetadataFile(jobID),
		PidFile:      session.SimplePidFile(jobID),
	}

	if opts.OnPrepared != nil {
		opts.OnPrepared(info)
	}

	// Check if session already exists
	exists, err := ssh.TmuxSessionExists(opts.Host, info.TmuxSession)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return deferJobToRemoteQueue(database, job, info, opts.EnvVars)
		}
		db.UpdateJobFailed(database, jobID, err.Error())
		return nil, fmt.Errorf("check session: %w", err)
	}

	if exists {
		db.UpdateJobFailed(database, jobID, "Session already exists")
		return nil, fmt.Errorf("session '%s' already exists on %s", info.TmuxSession, opts.Host)
	}

	// Create log directory on remote and archive any old files for this job
	mkdirAndArchive := fmt.Sprintf("mkdir -p %s; %s", session.LogDir, session.ArchiveCommand(jobID))
	if _, stderr, err := ssh.RunWithRetry(opts.Host, mkdirAndArchive); err != nil {
		if isConnectionFailure(stderr, err) {
			return deferJobToRemoteQueue(database, job, info, opts.EnvVars)
		}
		errMsg := ssh.FriendlyError(opts.Host, stderr, err)
		db.UpdateJobFailed(database, jobID, errMsg)
		return nil, fmt.Errorf("%s", errMsg)
	}

	// Save metadata
	metadata := session.FormatMetadata(jobID, info.WorkingDir, info.Command, info.Host, info.Description, job.StartTime)
	metadataCmd := fmt.Sprintf("cat > %s << 'METADATA_EOF'\n%s\nMETADATA_EOF", info.MetadataFile, metadata)
	if _, _, err := ssh.RunWithRetry(opts.Host, metadataCmd); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save metadata: %v\n", err)
	}

	result := &startJobResult{Info: info}

	// Slack notification setup
	notifyCmd := ""
	slackWebhook := slack.GetWebhook()
	if slackWebhook != "" {
		slack.DeployNotifyScript(opts.Host, slackWebhook)
		envVars := strings.TrimSpace(slack.BuildRunnerEnvPrefix(slackWebhook))
		notifyCmd = fmt.Sprintf("; %s '%s' 'rj-%d' $EXIT_CODE '%s' '%s'",
			envVars, slack.NotifyScriptPath, jobID, info.Host, info.MetadataFile)
		result.SlackEnabled = true
	}

	wrappedCommand := session.BuildWrapperCommand(session.WrapperCommandParams{
		JobID:      jobID,
		WorkingDir: info.WorkingDir,
		Command:    info.Command,
		LogFile:    info.LogFile,
		StatusFile: info.StatusFile,
		PidFile:    info.PidFile,
		NotifyCmd:  notifyCmd,
		Timeout:    opts.Timeout,
		EnvVars:    opts.EnvVars,
	})

	escapedCommand := ssh.EscapeForSingleQuotes(wrappedCommand)
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c '%s'", info.TmuxSession, escapedCommand)
	if _, stderr, err := ssh.Run(opts.Host, tmuxCmd); err != nil {
		if isConnectionFailure(stderr, err) {
			return deferJobToRemoteQueue(database, job, info, opts.EnvVars)
		}
		errMsg := ssh.FriendlyError(opts.Host, stderr, err)
		db.UpdateJobFailed(database, jobID, errMsg)
		return nil, fmt.Errorf("%s", errMsg)
	}

	if err := db.UpdateJobRunning(database, jobID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to update job status: %v\n", err)
	}

	return result, nil
}

func isConnectionFailure(stderr string, err error) bool {
	if stderr != "" && ssh.IsConnectionError(stderr) {
		return true
	}
	if err != nil && ssh.IsConnectionError(err.Error()) {
		return true
	}
	return false
}

func deferJobToRemoteQueue(database *sql.DB, job *db.Job, info StartJobPreparedInfo, envVars []string) (*startJobResult, error) {
	queueName := defaultQueueName

	if err := db.UpdateJobStartingToQueued(database, job.ID, queueName); err != nil {
		return nil, fmt.Errorf("mark job queued: %w", err)
	}

	return &startJobResult{
		Info:            info,
		DeferredToQueue: true,
	}, nil
}

type queueJobResult struct {
	JobID    int64
	Deferred bool
}

// queueJobOptions controls adding a job to a remote queue.
type queueJobOptions struct {
	Host         string
	WorkingDir   string
	Command      string
	Description  string
	EnvVars      []string
	Tags         []string
	GPU          string // Explicit GPU setting (extracted from EnvVars or set directly)
	QueueName    string
	Dependencies []queueDependency
	AutoStart    bool
}

type queueDependency struct {
	JobID        int64
	AllowFailure bool
}

// extractGPUFromEnvVars finds and returns the CUDA_VISIBLE_DEVICES value from env vars
func extractGPUFromEnvVars(envVars []string) string {
	for _, ev := range envVars {
		if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
			return strings.TrimPrefix(ev, "CUDA_VISIBLE_DEVICES=")
		}
	}
	return ""
}

func queueJob(database *sql.DB, opts queueJobOptions) (*queueJobResult, error) {
	queueName := defaultQueueName

	// Extract GPU from env vars if not explicitly set
	gpu := opts.GPU
	if gpu == "" {
		gpu = extractGPUFromEnvVars(opts.EnvVars)
	}

	depSpec := encodeQueueDependencies(opts.Dependencies)
	jobID, err := db.RecordQueuedWithGPU(database, opts.Host, opts.WorkingDir, opts.Command, opts.Description, queueName, gpu)
	if err != nil {
		return nil, fmt.Errorf("record job: %w", err)
	}
	if err := db.SetJobEnvVars(database, jobID, opts.EnvVars); err != nil {
		db.DeleteJob(database, jobID)
		return nil, fmt.Errorf("record env vars: %w", err)
	}
	if err := db.SetJobTags(database, jobID, opts.Tags); err != nil {
		db.DeleteJob(database, jobID)
		return nil, fmt.Errorf("record tags: %w", err)
	}
	if err := db.SetJobDepSpec(database, jobID, depSpec); err != nil {
		return nil, fmt.Errorf("record dependencies: %w", err)
	}

	entry := ops.QueueEntry{
		JobID:       jobID,
		WorkingDir:  opts.WorkingDir,
		Command:     opts.Command,
		Description: opts.Description,
		EnvVars:     opts.EnvVars,
		DepSpec:     depSpec,
	}
	// Use the new command queue system
	addCmd := ops.NewAddCommand(entry)
	if err := ops.AppendCommand(opts.Host, queueName, addCmd, ops.AppendCommandOptions{}); err != nil {
		if shouldDeferQueueAppend(err) {
			return &queueJobResult{JobID: jobID, Deferred: true}, nil
		}
		db.DeleteJob(database, jobID)
		return nil, err
	}

	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		return nil, fmt.Errorf("update sync state: %w", err)
	}

	return &queueJobResult{JobID: jobID}, nil
}

func shouldDeferQueueAppend(err error) bool {
	var qaErr *ops.QueueAppendError
	if errors.As(err, &qaErr) {
		return qaErr.IsConnectionError()
	}
	if err == nil {
		return false
	}
	return ssh.IsConnectionError(err.Error())
}

func applyEnvMap(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	vars := make([]string, 0, len(keys))
	for _, k := range keys {
		vars = append(vars, fmt.Sprintf("%s=%s", k, env[k]))
	}
	return vars
}

func encodeQueueDependencies(deps []queueDependency) string {
	if len(deps) == 0 {
		return ""
	}
	parts := make([]string, 0, len(deps))
	for _, dep := range deps {
		if dep.JobID <= 0 {
			continue
		}
		part := fmt.Sprintf("%d", dep.JobID)
		if dep.AllowFailure {
			part += ":any"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

func ensureSameHostDependency(database *sql.DB, depID int64, host string) error {
	if depID <= 0 {
		return fmt.Errorf("invalid dependency job ID %d", depID)
	}
	job, err := db.GetJobByID(database, depID)
	if err != nil {
		return fmt.Errorf("lookup dependency job %d: %w", depID, err)
	}
	if job == nil {
		return fmt.Errorf("dependency job %d not found", depID)
	}
	if job.Host != host {
		return fmt.Errorf("dependency job %d runs on host %s; cannot depend on it from host %s", depID, job.Host, host)
	}
	return nil
}
