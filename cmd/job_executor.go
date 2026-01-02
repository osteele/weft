package cmd

import (
	"database/sql"
	"encoding/json"
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
		LogFile:      session.LogFile(jobID, job.StartTime),
		StatusFile:   session.StatusFile(jobID, job.StartTime),
		MetadataFile: session.MetadataFile(jobID, job.StartTime),
		PidFile:      session.PidFile(jobID, job.StartTime),
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

	// Create log directory on remote
	logDir := session.LogDir
	mkdirCmd := fmt.Sprintf("mkdir -p %s", logDir)
	if _, stderr, err := ssh.RunWithRetry(opts.Host, mkdirCmd); err != nil {
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

type deferredQueuePayload struct {
	WorkingDir  string   `json:"working_dir"`
	Command     string   `json:"command"`
	Description string   `json:"description,omitempty"`
	EnvVars     []string `json:"env_vars,omitempty"`
	QueueName   string   `json:"queue_name"`
	DepSpec     string   `json:"dep_spec,omitempty"`
	AutoStart   bool     `json:"auto_start_runner,omitempty"`
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
	queueName := job.QueueName
	if queueName == "" {
		queueName = defaultQueueName
	}

	payload := deferredQueuePayload{
		WorkingDir:  job.WorkingDir,
		Command:     job.Command,
		Description: job.Description,
		EnvVars:     envVars,
		QueueName:   queueName,
		AutoStart:   true,
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode deferred queue payload: %w", err)
	}

	if err := db.UpdateJobStartingToQueued(database, job.ID, queueName); err != nil {
		return nil, fmt.Errorf("mark job queued: %w", err)
	}

	if err := db.AddDeferredOperation(database, job.Host, db.OpQueueJob, job.ID, queueName, string(payloadJSON)); err != nil {
		return nil, fmt.Errorf("add deferred operation: %w", err)
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
	queueName := opts.QueueName
	if queueName == "" {
		queueName = defaultQueueName
	}

	// Extract GPU from env vars if not explicitly set
	gpu := opts.GPU
	if gpu == "" {
		gpu = extractGPUFromEnvVars(opts.EnvVars)
	}

	jobID, err := db.RecordQueuedWithGPU(database, opts.Host, opts.WorkingDir, opts.Command, opts.Description, queueName, gpu)
	if err != nil {
		return nil, fmt.Errorf("record job: %w", err)
	}

	depSpec := encodeQueueDependencies(opts.Dependencies)
	entry := ops.QueueEntry{
		JobID:       jobID,
		WorkingDir:  opts.WorkingDir,
		Command:     opts.Command,
		Description: opts.Description,
		EnvVars:     opts.EnvVars,
		DepSpec:     depSpec,
	}
	if err := ops.AppendQueueEntry(opts.Host, queueName, entry, ops.AppendQueueEntryOptions{}); err != nil {
		if shouldDeferQueueAppend(err) {
			if err := deferQueueAppend(database, opts, queueName, jobID, depSpec); err != nil {
				db.DeleteJob(database, jobID)
				return nil, err
			}
			return &queueJobResult{JobID: jobID, Deferred: true}, nil
		}
		db.DeleteJob(database, jobID)
		return nil, err
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

func deferQueueAppend(database *sql.DB, opts queueJobOptions, queueName string, jobID int64, depSpec string) error {
	payload := deferredQueuePayload{
		WorkingDir:  opts.WorkingDir,
		Command:     opts.Command,
		Description: opts.Description,
		EnvVars:     opts.EnvVars,
		QueueName:   queueName,
		DepSpec:     depSpec,
	}

	if opts.AutoStart && depSpec == "" {
		payload.AutoStart = true
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode deferred queue payload: %w", err)
	}

	if err := db.AddDeferredOperation(database, opts.Host, db.OpQueueJob, jobID, queueName, string(payloadJSON)); err != nil {
		return fmt.Errorf("add deferred operation: %w", err)
	}
	return nil
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
