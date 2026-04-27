package dashboard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/coordinatorrelay"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/logfiles"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/progress"
	"github.com/osteele/weft/internal/ssh"
)

func (m Model) createJob() tea.Cmd {
	database := m.database
	host := strings.TrimSpace(m.inputs[inputHost].Value())
	command := strings.TrimSpace(m.inputs[inputCommand].Value())
	description := strings.TrimSpace(m.inputs[inputDescription].Value())
	workingDir := strings.TrimSpace(m.inputs[inputWorkingDir].Value())
	envVarsStr := strings.TrimSpace(m.inputs[inputEnvVars].Value())
	gpuInput := strings.TrimSpace(m.inputs[inputGPU].Value())
	cpuInput := strings.TrimSpace(m.inputs[inputCPUAllotment].Value())
	cpuAllotment, cpuErr := parseCPUAllotmentInput(cpuInput)

	// Normalize command: extract cd/env prefixes into proper fields
	// This allows users to paste commands like "cd /foo && env CUDA=0 python train.py"
	normalizedDir, normalizedCmd, normalizedEnv := db.NormalizeCommand(command)
	if normalizedDir != "" && workingDir == "" {
		workingDir = normalizedDir
		command = normalizedCmd
	}

	if workingDir == "" {
		workingDir = "~"
	}

	// Parse env vars (comma-separated VAR=value pairs from form field)
	envVars := parseEnvInput(envVarsStr)

	// Append env vars extracted from command (if we didn't use them above)
	if normalizedDir != "" {
		// We extracted cd, so also use the normalized env vars
		envVars = append(envVars, normalizedEnv...)
	} else if len(normalizedEnv) > 0 {
		// No cd prefix, but we have env vars in the command - use them
		command = normalizedCmd
		envVars = append(envVars, normalizedEnv...)
	}

	// Merge GPU field with env vars
	existingGPU, envVars := splitGPUEnvVars(envVars)
	if gpuInput == "" {
		gpuInput = existingGPU
	}
	envVars = mergeGPUEnvVars(envVars, gpuInput)

	return func() tea.Msg {
		if cpuErr != nil {
			return jobCreatedMsg{err: cpuErr}
		}
		predCfg := placement.PredictorConfigFromApp(m.appConfig)
		projectName := db.DeriveProject(workingDir, command)
		gpuMemGB, gpuMemMaxGB, _ := predictor.ResolveGPUMem(predCfg, nil, gpuInput != "", host, projectName, "", command, ops.DefaultGPUMemGB, 0)
		params := ops.QueueJobParams{
			Host:         host,
			WorkingDir:   workingDir,
			Command:      command,
			Description:  description,
			EnvVars:      envVars,
			CPUAllotment: cpuAllotment,
			GPUMemGB:     gpuMemGB,
			GPUMemMaxGB:  gpuMemMaxGB,
		}
		if _, relayClient, err := m.coordinatorRelay(); err != nil {
			return jobCreatedMsg{err: err}
		} else if relayClient != nil {
			jobID, _, err := m.relaySubmitJob(database, params)
			if err != nil {
				return jobCreatedMsg{err: err}
			}
			return jobCreatedMsg{jobID: jobID}
		}
		// Queue job for sequential execution via queue runner
		result, err := ops.QueueJob(database, params, ops.DefaultOptions())

		if err != nil {
			return jobCreatedMsg{err: err}
		}

		return jobCreatedMsg{
			jobID:    result.JobID,
			deferred: result.Deferred,
		}
	}
}

func (m Model) editJob() tea.Cmd {
	database := m.database
	jobID := m.editingJobID
	newHost := strings.TrimSpace(m.inputs[inputHost].Value())
	newCommand := strings.TrimSpace(m.inputs[inputCommand].Value())
	newDescription := strings.TrimSpace(m.inputs[inputDescription].Value())
	newWorkingDir := strings.TrimSpace(m.inputs[inputWorkingDir].Value())
	envVarsStr := strings.TrimSpace(m.inputs[inputEnvVars].Value())
	gpuInput := strings.TrimSpace(m.inputs[inputGPU].Value())
	cpuInput := strings.TrimSpace(m.inputs[inputCPUAllotment].Value())

	return func() tea.Msg {
		newAllotment, err := parseCPUAllotmentInput(cpuInput)
		if err != nil {
			return jobEditedMsg{jobID: jobID, err: err}
		}
		// Get the current job to check status and get original host
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return jobEditedMsg{jobID: jobID, err: fmt.Errorf("get job: %w", err)}
		}
		if job == nil {
			return jobEditedMsg{jobID: jobID, err: fmt.Errorf("job %s not found", ids.FormatJobID(jobID))}
		}
		if job.EffectiveStatus() != db.StatusQueued {
			return jobEditedMsg{jobID: jobID, host: job.Host, err: fmt.Errorf("can only edit queued jobs")}
		}

		// Check if host is being changed - not allowed in edit, use job move instead
		if newHost != job.Host {
			return jobEditedMsg{jobID: jobID, host: job.Host, err: fmt.Errorf("cannot change host via edit; use 'job move' command")}
		}

		// Update fields in database
		if newDescription != job.Description {
			if err := db.UpdateJobDescription(database, jobID, newDescription); err != nil {
				return jobEditedMsg{jobID: jobID, host: job.Host, err: fmt.Errorf("update description: %w", err)}
			}
		}

		if newWorkingDir != job.WorkingDir {
			if err := db.UpdateJobWorkingDir(database, jobID, newWorkingDir); err != nil {
				return jobEditedMsg{jobID: jobID, host: job.Host, err: fmt.Errorf("update directory: %w", err)}
			}
		}

		if newCommand != job.Command {
			if err := db.UpdateJobCommand(database, jobID, newCommand); err != nil {
				return jobEditedMsg{jobID: jobID, host: job.Host, err: fmt.Errorf("update command: %w", err)}
			}
		}

		// Prepare env vars and GPU
		envVars := parseEnvInput(envVarsStr)
		existingGPU, envVars := splitGPUEnvVars(envVars)
		if gpuInput == "" {
			gpuInput = existingGPU
		}
		envVars = mergeGPUEnvVars(envVars, gpuInput)

		descriptionChanged := newDescription != job.Description
		workingDirChanged := newWorkingDir != job.WorkingDir
		commandChanged := newCommand != job.Command
		envChanged := !equalEnvVars(envVars, job.EnvVars)
		gpuChanged := gpuInput != job.GPU
		cpuChanged := !equalCPUAllotment(newAllotment, job.CPUAllotment)
		operationalChange := workingDirChanged || commandChanged || gpuChanged || envChanged || cpuChanged

		// Update the remote queue file
		depSpec := m.editingJobDepSpec
		if depSpec == "" {
			depSpec = job.DepSpec
		}
		depSpecChanged := depSpec != job.DepSpec
		if envChanged {
			if err := db.SetJobEnvVars(database, jobID, envVars); err != nil {
				return jobEditedMsg{jobID: jobID, host: job.Host, err: fmt.Errorf("update env vars: %w", err)}
			}
		}
		if gpuChanged {
			if err := db.SetJobGPU(database, jobID, gpuInput); err != nil {
				return jobEditedMsg{jobID: jobID, host: job.Host, err: fmt.Errorf("update GPU: %w", err)}
			}
		}
		if cpuChanged {
			if err := db.SetJobCPUAllotment(database, jobID, newAllotment); err != nil {
				return jobEditedMsg{jobID: jobID, host: job.Host, err: fmt.Errorf("update CPU allotment: %w", err)}
			}
		}

		job.Command = newCommand
		job.WorkingDir = newWorkingDir
		job.Description = newDescription
		job.EnvVars = envVars
		job.GPU = gpuInput
		job.CPUAllotment = newAllotment
		job.DepSpec = depSpec

		if descriptionChanged || operationalChange {
			if _, relayClient, err := m.coordinatorRelay(); err != nil {
				return jobEditedMsg{jobID: jobID, host: job.Host, err: err}
			} else if relayClient != nil {
				payload := &coordinatorrelay.UpdateJobPayload{}
				if descriptionChanged {
					payload.Description = &job.Description
				}
				if workingDirChanged {
					payload.WorkingDir = &job.WorkingDir
				}
				if commandChanged {
					payload.Command = &job.Command
				}
				if envChanged {
					payload.EnvVars = job.EnvVars
					if len(job.EnvVars) == 0 {
						payload.ClearEnv = true
					}
				}
				if gpuChanged {
					payload.GPU = tuiStringPtr(job.GPU)
				}
				if cpuChanged {
					payload.CPUAllotment = job.CPUAllotment
				}
				if depSpecChanged {
					payload.DepSpec = &job.DepSpec
				}
				if _, err := m.relayUpdateJob(job, payload); err != nil {
					return jobEditedMsg{jobID: jobID, host: job.Host, err: err}
				}
				return jobEditedMsg{jobID: jobID, host: job.Host}
			}
		}

		if operationalChange {
			result, err := ops.RequestQueueUpdate(database, job, ops.OptionsForMode(ops.TimeoutFast))
			if err != nil {
				return jobEditedMsg{jobID: jobID, host: job.Host, err: err}
			}
			return jobEditedMsg{jobID: jobID, host: job.Host, deferred: result.Deferred}
		}

		return jobEditedMsg{jobID: jobID, host: job.Host}
	}
}

type queueEntryData struct {
	envVars []string
	depSpec string
}

func fetchQueueEntryData(database *sql.DB, jobID int64) (*queueEntryData, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return &queueEntryData{}, nil
	}
	return &queueEntryData{
		envVars: job.EnvVars,
		depSpec: job.DepSpec,
	}, nil
}

func (m Model) fetchQueuedJobEnv(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	jobID := job.ID
	return func() tea.Msg {
		data, err := fetchQueueEntryData(database, jobID)
		if err != nil {
			return jobEnvLoadedMsg{jobID: jobID, err: err}
		}
		if data == nil {
			data = &queueEntryData{}
		}
		envCopy := append([]string(nil), data.envVars...)
		return jobEnvLoadedMsg{
			jobID:   jobID,
			envVars: envCopy,
			depSpec: data.depSpec,
		}
	}
}

func (m Model) fetchJobLog(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}

	jobCopy := *job
	ctx := m.ctx
	return func() tea.Msg {
		job := jobCopy
		// For terminal jobs, try local cache first
		if db.IsTerminalStatus(job.EffectiveStatus()) {
			if cached, err := logcache.Read(job.ID); err == nil {
				// Apply tail -500 equivalent
				lines := strings.Split(cached, "\n")
				if len(lines) > 500 {
					lines = lines[len(lines)-500:]
				}
				content := strings.Join(lines, "\n")
				prog := progress.FindLastProgressPreferExplicit(content)
				return logFetchedMsg{
					jobID:     job.ID,
					content:   content,
					progress:  prog,
					fromCache: true,
				}
			}
			// Fall through to remote fetch if not cached
		}

		logFile, _ := logfiles.Resolve(&job)

		// Fetch the log content
		// Don't quote path - it contains ~ which needs shell expansion
		stdout, stderr, err := ssh.TryRunWithContext(ctx, job.Host, fmt.Sprintf("tail -500 %s 2>&1", logFile))
		if err != nil {
			// Pool busy or context cancelled - exit silently
			if errors.Is(err, ssh.ErrPoolBusy) || err == context.Canceled {
				return nil
			}
			// Check if it's a connection error
			combined := stdout + stderr
			if ssh.IsConnectionError(combined) {
				if cached, err := logcache.Read(job.ID); err == nil {
					return logFetchedMsg{
						jobID:     job.ID,
						content:   cached,
						connError: true,
						fromCache: true,
					}
				}
				return logFetchedMsg{
					jobID:     job.ID,
					content:   fmt.Sprintf("Host %s unreachable", job.Host),
					connError: true,
				}
			}
			// Check if log file doesn't exist
			if strings.Contains(combined, "No such file") || strings.Contains(combined, "cannot open") {
				msg := "No log file yet"
				if db.IsTerminalStatus(job.EffectiveStatus()) {
					msg = "Log file not found (may have been cleaned up)"
				}
				return logFetchedMsg{
					jobID:   job.ID,
					content: msg,
				}
			}
			// Other SSH error
			return logFetchedMsg{
				jobID:   job.ID,
				content: fmt.Sprintf("Error: %s", strings.TrimSpace(combined)),
			}
		}
		// Check if output indicates file not found (for cases where tail doesn't error)
		if strings.Contains(stdout, "No such file") || strings.Contains(stdout, "cannot open") {
			msg := "No log file yet"
			if db.IsTerminalStatus(job.EffectiveStatus()) {
				msg = "Log file not found (may have been cleaned up)"
			}
			return logFetchedMsg{
				jobID:   job.ID,
				content: msg,
			}
		}

		// Extract progress information from log content
		prog := progress.FindLastProgressPreferExplicit(stdout)

		return logFetchedMsg{
			jobID:    job.ID,
			content:  stdout,
			progress: prog,
		}
	}
}

func (m Model) fetchSelectedJobLog() tea.Cmd {
	if m.selectedJob == nil {
		return nil
	}
	return m.fetchJobLog(m.selectedJob)
}

// fetchQuickProgress quickly greps the log file for the last progress line
// This is faster than fetching the full log and is used for periodic refresh
func (m Model) fetchQuickProgress(job *db.Job) tea.Cmd {
	if job == nil || job.EffectiveStatus() != db.StatusRunning {
		return nil
	}

	ctx := m.ctx
	return func() tea.Msg {
		// Find the log file via shared resolver
		logFile, _ := logfiles.Resolve(job)

		grepCmd := progress.GrepCommand(logFile)
		stdout, _, err := ssh.RunWithContext(ctx, job.Host, grepCmd)
		if err != nil || strings.TrimSpace(stdout) == "" {
			return quickProgressMsg{jobID: job.ID, progress: nil}
		}

		prog := progress.FindLastProgressPreferExplicit(strings.TrimSpace(stdout))
		return quickProgressMsg{jobID: job.ID, progress: prog}
	}
}

// fetchAllRunningJobsProgress fetches progress for all running jobs quickly
func (m Model) fetchAllRunningJobsProgress() tea.Cmd {
	var cmds []tea.Cmd
	for _, job := range m.allJobs {
		if job.EffectiveStatus() == db.StatusRunning && !job.Tombstoned {
			if cmd := m.fetchQuickProgress(job); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}
