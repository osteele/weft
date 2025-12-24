package cmd

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
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
	QueueOnFail bool
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
	Info                      StartJobPreparedInfo
	SlackEnabled              bool
	QueuedOnConnectionFailure bool
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
		if ssh.IsConnectionError(err.Error()) && opts.QueueOnFail {
			if err := db.UpdateJobPending(database, jobID); err != nil {
				return nil, fmt.Errorf("queue job: %w", err)
			}
			return &startJobResult{Info: info, QueuedOnConnectionFailure: true}, nil
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
		if ssh.IsConnectionError(stderr) && opts.QueueOnFail {
			if err := db.UpdateJobPending(database, jobID); err != nil {
				return nil, fmt.Errorf("queue job: %w", err)
			}
			return &startJobResult{Info: info, QueuedOnConnectionFailure: true}, nil
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
	slackWebhook := getSlackWebhook()
	if slackWebhook != "" {
		remoteNotifyScript := "/tmp/remote-jobs-notify-slack.sh"
		writeCmd := fmt.Sprintf("cat > '%s' << 'SCRIPT_EOF'\n%s\nSCRIPT_EOF", remoteNotifyScript, string(notifySlackScript))
		if _, stderr, err := ssh.RunWithRetry(opts.Host, writeCmd); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write notify script: %s\n", stderr)
		} else {
			if _, stderr, err := ssh.Run(opts.Host, fmt.Sprintf("chmod +x '%s'", remoteNotifyScript)); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to chmod notify script: %s\n", stderr)
			} else {
				envVars := fmt.Sprintf("REMOTE_JOBS_SLACK_WEBHOOK='%s'", slackWebhook)
				if v := os.Getenv("REMOTE_JOBS_SLACK_VERBOSE"); v == "1" {
					envVars += " REMOTE_JOBS_SLACK_VERBOSE=1"
				}
				if v := os.Getenv("REMOTE_JOBS_SLACK_NOTIFY"); v != "" {
					envVars += fmt.Sprintf(" REMOTE_JOBS_SLACK_NOTIFY='%s'", v)
				}
				if v := os.Getenv("REMOTE_JOBS_SLACK_MIN_DURATION"); v != "" {
					envVars += fmt.Sprintf(" REMOTE_JOBS_SLACK_MIN_DURATION='%s'", v)
				}
				notifyCmd = fmt.Sprintf("; %s '%s' 'rj-%d' $EXIT_CODE '%s' '%s'",
					envVars, remoteNotifyScript, jobID, info.Host, info.MetadataFile)
				result.SlackEnabled = true
			}
		}
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
		if ssh.IsConnectionError(stderr) && opts.QueueOnFail {
			if err := db.UpdateJobPending(database, jobID); err != nil {
				return nil, fmt.Errorf("queue job: %w", err)
			}
			return &startJobResult{Info: info, QueuedOnConnectionFailure: true}, nil
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

// queueJobOptions controls adding a job to a remote queue.
type queueJobOptions struct {
	Host        string
	WorkingDir  string
	Command     string
	Description string
	EnvVars     []string
	QueueName   string
	AfterJobID  int64
	AfterAny    bool
}

func queueJob(database *sql.DB, opts queueJobOptions) (int64, error) {
	queueName := opts.QueueName
	if queueName == "" {
		queueName = defaultQueueName
	}

	jobID, err := db.RecordQueued(database, opts.Host, opts.WorkingDir, opts.Command, opts.Description, queueName)
	if err != nil {
		return 0, fmt.Errorf("record job: %w", err)
	}

	mkdirCmd := fmt.Sprintf("mkdir -p %s", queueDir)
	if _, stderr, err := ssh.Run(opts.Host, mkdirCmd); err != nil {
		db.DeleteJob(database, jobID)
		return 0, fmt.Errorf("create queue directory: %s", stderr)
	}

	queueFile := fmt.Sprintf("%s/%s.queue", queueDir, queueName)
	envVarsB64 := ""
	if len(opts.EnvVars) > 0 {
		envVarsB64 = base64.StdEncoding.EncodeToString([]byte(strings.Join(opts.EnvVars, "\n")))
	}
	afterJobStr := ""
	if opts.AfterJobID > 0 {
		afterJobStr = fmt.Sprintf("%d", opts.AfterJobID)
		if opts.AfterAny {
			afterJobStr = fmt.Sprintf("%d:any", opts.AfterJobID)
		}
	}
	jobLine := fmt.Sprintf("%d\t%s\t%s\t%s\t%s\t%s", jobID, opts.WorkingDir, opts.Command, opts.Description, envVarsB64, afterJobStr)
	appendCmd := fmt.Sprintf("echo '%s' >> %s", ssh.EscapeForSingleQuotes(jobLine), queueFile)
	if _, stderr, err := ssh.Run(opts.Host, appendCmd); err != nil {
		db.DeleteJob(database, jobID)
		return 0, fmt.Errorf("append to queue: %s", stderr)
	}

	return jobID, nil
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
