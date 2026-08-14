package skypilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
)

type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

type StreamingRunner interface {
	RunStreaming(ctx context.Context, stdout, stderr io.Writer, args ...string) error
}

type CLIRunner struct{}

func (CLIRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sky", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("sky %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (CLIRunner) RunStreaming(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "sky", args...)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open sky stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open sky stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sky %s: %w", strings.Join(args, " "), err)
	}
	type copyResult struct {
		stream string
		err    error
	}
	copyDone := make(chan copyResult, 2)
	go func() {
		_, err := io.Copy(stdout, stdoutPipe)
		copyDone <- copyResult{stream: "stdout", err: err}
	}()
	go func() {
		_, err := io.Copy(stderr, stderrPipe)
		copyDone <- copyResult{stream: "stderr", err: err}
	}()
	var copyErr error
	for range 2 {
		result := <-copyDone
		if result.err != nil && copyErr == nil {
			copyErr = fmt.Errorf("stream sky %s: %w", result.stream, result.err)
			_ = cmd.Process.Kill()
		}
	}
	waitErr := cmd.Wait()
	if copyErr != nil {
		return copyErr
	}
	if waitErr != nil {
		return fmt.Errorf("sky %s: %w", strings.Join(args, " "), waitErr)
	}
	return nil
}

type Client struct {
	Runner   Runner
	LookPath func(string) (string, error)
}

var ErrNotConfigured = errors.New("SkyPilot CLI is not configured")

// SubmissionRefusedError is positive evidence that an external submit was
// rejected before SkyPilot accepted it. Generic CLI failures are not this:
// they may occur after request acceptance and therefore have unknown outcome.
type SubmissionRefusedError struct {
	Cause error
}

func (e *SubmissionRefusedError) Error() string {
	if e == nil || e.Cause == nil {
		return "SkyPilot submission was refused"
	}
	return "SkyPilot submission was refused: " + e.Cause.Error()
}

func (e *SubmissionRefusedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// SubmissionUnconfirmedError means the external submit was attempted but its
// outcome cannot be proved. The task may be running and billing, so callers
// must keep the local attempt nonterminal until an external identity can be
// recovered.
type SubmissionUnconfirmedError struct {
	Cause error
}

func (e *SubmissionUnconfirmedError) Error() string {
	if e == nil || e.Cause == nil {
		return "SkyPilot submission outcome is unconfirmed"
	}
	return "SkyPilot submission outcome is unconfirmed: " + e.Cause.Error()
}

func (e *SubmissionUnconfirmedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (c Client) IsConfigured() bool {
	if c.LookPath != nil {
		_, err := c.LookPath("sky")
		return err == nil
	}
	if c.Runner != nil {
		// An injected runner is an explicitly configured adapter.
		return true
	}
	_, err := exec.LookPath("sky")
	return err == nil
}

type Job struct {
	ID          string
	TaskID      string
	TaskName    string
	Name        string
	Status      string
	Message     string
	ClusterID   string
	ClusterName string
	Command     string
	Dashboard   string
	SubmittedAt *int64
	StartedAt   *int64
	EndedAt     *int64
}

func (c Client) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return CLIRunner{}
}

func (c Client) ListJobs(ctx context.Context) ([]Job, error) {
	out, err := c.runner().Run(ctx, "jobs", "queue", "--all", "--verbose", "--output", "json")
	if err != nil {
		return nil, err
	}
	return ParseJobsQueueJSON(out)
}

// StreamLogs keeps SkyPilot stdout separate from diagnostics and emits data as
// the CLI produces it. Callers that request a full retained log therefore do
// not need to buffer it in memory.
func (c Client) StreamLogs(ctx context.Context, externalJobID, externalTaskID string, follow bool, tail int, stdout, stderr io.Writer) error {
	if tail < 0 {
		return fmt.Errorf("SkyPilot log tail must be nonnegative")
	}
	args := logArgs(externalJobID, externalTaskID, follow, tail)
	runner := c.runner()
	if streaming, ok := runner.(StreamingRunner); ok {
		return streaming.RunStreaming(ctx, stdout, stderr, args...)
	}
	out, err := runner.Run(ctx, args...)
	if len(out) > 0 {
		if _, writeErr := stdout.Write(out); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func (c Client) FollowLogs(ctx context.Context, externalJobID, externalTaskID string, tail int, stdout, stderr io.Writer) error {
	return c.StreamLogs(ctx, externalJobID, externalTaskID, true, tail, stdout, stderr)
}

func logArgs(externalJobID, externalTaskID string, follow bool, tail int) []string {
	args := []string{"jobs", "logs", externalJobID}
	if strings.TrimSpace(externalTaskID) != "" {
		args = append(args, strings.TrimSpace(externalTaskID))
	}
	if follow {
		args = append(args, "--follow")
	} else {
		args = append(args, "--no-follow")
	}
	args = append(args, "--tail", strconv.Itoa(tail))
	return args
}

func (c Client) Cancel(ctx context.Context, externalJobID string) error {
	_, err := c.runner().Run(ctx, "jobs", "cancel", "-y", externalJobID)
	return err
}

type SubmitOptions struct {
	Name       string
	Command    string
	WorkDir    string
	GPUClass   string
	GPUCount   int
	GPUMemGB   *int
	EnvVars    []string
	DryRunPath string
}

func (c Client) Submit(ctx context.Context, opts SubmitOptions) (*Job, string, error) {
	task, err := BuildTaskYAML(opts)
	if err != nil {
		return nil, "", err
	}
	if opts.DryRunPath != "" {
		if err := os.WriteFile(opts.DryRunPath, []byte(task), 0o644); err != nil {
			return nil, "", err
		}
		return nil, opts.DryRunPath, nil
	}
	f, err := os.CreateTemp("", "weft-sky-*.yaml")
	if err != nil {
		return nil, "", err
	}
	taskPath := f.Name()
	if _, err := f.WriteString(task); err != nil {
		f.Close()
		os.Remove(taskPath)
		return nil, "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(taskPath)
		return nil, "", err
	}
	defer os.Remove(taskPath)

	out, err := c.runner().Run(ctx, "jobs", "launch", "-y", "-d", "--name", opts.Name, taskPath)
	if err != nil {
		var refused *SubmissionRefusedError
		if errors.As(err, &refused) {
			return nil, taskPath, err
		}
		if errors.Is(err, exec.ErrNotFound) {
			return nil, taskPath, &SubmissionRefusedError{Cause: ErrNotConfigured}
		}
		// A nonzero CLI result does not prove the request was rejected: the
		// client or API can fail after SkyPilot accepts the launch.
		return nil, taskPath, &SubmissionUnconfirmedError{Cause: err}
	}
	// Successful detached launches print the authoritative managed-job ID.
	// Resolve that identity before considering the generated name: an older
	// job can legitimately have the same name while the fresh queue snapshot
	// has not exposed the new launch yet.
	submittedID := submittedJobID(string(out))
	jobs, listErr := c.ListJobs(ctx)
	if submittedID != "" {
		if listErr == nil {
			job, resolveErr := resolveJobByExactID(jobs, submittedID)
			if resolveErr == nil && job != nil {
				return job, taskPath, nil
			}
		}
		return &Job{ID: submittedID, Name: opts.Name, Status: "submitted"}, taskPath, nil
	}
	if listErr == nil {
		job, resolveErr := ResolveJobByName(jobs, opts.Name)
		if resolveErr == nil && job != nil {
			return job, taskPath, nil
		}
		if resolveErr != nil {
			listErr = resolveErr
		}
	}
	if listErr != nil {
		return nil, taskPath, &SubmissionUnconfirmedError{Cause: fmt.Errorf("submitted SkyPilot task but could not resolve external job id: %w", listErr)}
	}
	return nil, taskPath, &SubmissionUnconfirmedError{Cause: fmt.Errorf("submitted SkyPilot task but could not resolve external job id")}
}

func resolveJobByExactID(jobs []Job, externalID string) (*Job, error) {
	externalID = strings.TrimSpace(externalID)
	var match *Job
	for i := range jobs {
		if strings.TrimSpace(jobs[i].ID) != externalID {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("SkyPilot job ID %q matched multiple tasks", externalID)
		}
		match = &jobs[i]
	}
	return match, nil
}

func BuildTaskYAML(opts SubmitOptions) (string, error) {
	if strings.TrimSpace(opts.Command) == "" {
		return "", fmt.Errorf("command is required")
	}
	if opts.GPUMemGB != nil {
		return "", fmt.Errorf("standalone GPU memory constraints are not supported by SkyPilot task YAML")
	}
	var b strings.Builder
	if opts.Name != "" {
		fmt.Fprintf(&b, "name: %s\n", quoteYAML(opts.Name))
	}
	if opts.WorkDir != "" {
		fmt.Fprintf(&b, "workdir: %s\n", quoteYAML(opts.WorkDir))
	}
	if opts.GPUClass != "" {
		b.WriteString("resources:\n")
		count := opts.GPUCount
		if count <= 0 {
			count = 1
		}
		fmt.Fprintf(&b, "  accelerators: %s:%d\n", strings.ToUpper(opts.GPUClass), count)
	}
	if len(opts.EnvVars) > 0 {
		b.WriteString("envs:\n")
		for _, kv := range opts.EnvVars {
			key, value, ok := strings.Cut(kv, "=")
			if !ok || strings.TrimSpace(key) == "" {
				return "", fmt.Errorf("invalid env var %q; expected KEY=VALUE", kv)
			}
			fmt.Fprintf(&b, "  %s: %s\n", key, quoteYAML(value))
		}
	}
	fmt.Fprintf(&b, "run: %s\n", blockYAML(opts.Command))
	return b.String(), nil
}

func quoteYAML(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}

func blockYAML(s string) string {
	if !strings.Contains(s, "\n") {
		return quoteYAML(s)
	}
	var b strings.Builder
	b.WriteString("|\n")
	for _, line := range strings.Split(s, "\n") {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	return b.String()
}

func ParseJobsQueueJSON(data []byte) ([]Job, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	var rows []any
	switch v := payload.(type) {
	case []any:
		rows = v
	case map[string]any:
		for _, key := range []string{"jobs", "tasks", "managed_jobs", "queue"} {
			if a, ok := v[key].([]any); ok {
				rows = a
				break
			}
		}
		if rows == nil {
			rows = []any{v}
		}
	default:
		return nil, fmt.Errorf("unexpected SkyPilot JSON shape %T", payload)
	}
	jobs := make([]Job, 0, len(rows))
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		jobs = append(jobs, parseJobMap(m))
	}
	return jobs, nil
}

func parseJobMap(m map[string]any) Job {
	return Job{
		ID:          firstString(m, "job_id", "job_id_int", "id", "job"),
		TaskID:      firstString(m, "task_id", "task"),
		TaskName:    firstString(m, "task_name"),
		Name:        firstString(m, "name", "job_name"),
		Status:      firstString(m, "status", "state", "job_status"),
		Message:     firstString(m, "failure_reason", "details", "status_message", "message"),
		ClusterID:   firstString(m, "cluster_id"),
		ClusterName: firstString(m, "current_cluster_name", "cluster", "cluster_name", "cluster_id"),
		Command:     firstString(m, "entrypoint", "command", "run", "run_command"),
		Dashboard:   firstURL(m, "dashboard_url", "url", "links"),
		SubmittedAt: firstUnixSeconds(m, "submitted_at"),
		StartedAt:   firstUnixSeconds(m, "start_at"),
		EndedAt:     firstUnixSeconds(m, "end_at"),
	}
}

func firstUnixSeconds(m map[string]any, key string) *int64 {
	value, ok := m[key]
	if !ok || value == nil {
		return nil
	}
	seconds, err := strconv.ParseFloat(valueString(value), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > math.MaxInt64 {
		return nil
	}
	unix := int64(seconds)
	return &unix
}

func firstURL(m map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := m[key]
		if !ok {
			continue
		}
		if s := valueString(value); s != "" {
			return s
		}
		links, ok := value.(map[string]any)
		if !ok {
			continue
		}
		labels := make([]string, 0, len(links))
		for label := range links {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		for _, label := range labels {
			if s := valueString(links[label]); s != "" {
				return s
			}
		}
	}
	return ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			if s := valueString(value); s != "" {
				return s
			}
		}
	}
	return ""
}

func valueString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	default:
		return ""
	}
}

func ResolveJob(jobs []Job, externalID string) (*Job, error) {
	externalID = strings.TrimSpace(externalID)
	for _, field := range []func(Job) string{
		func(job Job) string { return job.ID },
		func(job Job) string { return job.Name },
	} {
		var match *Job
		for i := range jobs {
			if strings.TrimSpace(field(jobs[i])) != externalID {
				continue
			}
			if match != nil {
				return nil, fmt.Errorf("SkyPilot identity %q matched multiple jobs", externalID)
			}
			match = &jobs[i]
		}
		if match != nil {
			return match, nil
		}
	}
	return nil, nil
}

// ResolveJobWithTask selects a complete managed-job/task identity. Without a
// task selector it preserves ResolveJob's compatibility lookup; with one, the
// managed-job ID must match exactly and the task selector is resolved only
// within that job.
func ResolveJobWithTask(jobs []Job, externalID, task string) (*Job, error) {
	task = strings.TrimSpace(task)
	if task == "" {
		return ResolveJob(jobs, externalID)
	}
	externalID = strings.TrimSpace(externalID)
	var match *Job
	for i := range jobs {
		if strings.TrimSpace(jobs[i].ID) != externalID {
			continue
		}
		if strings.TrimSpace(jobs[i].TaskID) != task && strings.TrimSpace(jobs[i].TaskName) != task {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("SkyPilot identity %q task %q matched multiple jobs", externalID, task)
		}
		match = &jobs[i]
	}
	return match, nil
}

func ResolveJobByName(jobs []Job, name string) (*Job, error) {
	name = strings.TrimSpace(name)
	var match *Job
	for i := range jobs {
		if strings.TrimSpace(jobs[i].Name) != name {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("SkyPilot task name %q matched multiple jobs", name)
		}
		match = &jobs[i]
	}
	return match, nil
}

func ObservationFromJob(job Job, project, workingDir string) db.ExternalJobObservation {
	id := job.ID
	if id == "" {
		id = job.Name
	}
	return db.ExternalJobObservation{
		Executor:                db.ExternalExecutorSkyPilot,
		ExternalJobID:           id,
		ExternalTaskID:          job.TaskID,
		ExternalClusterID:       job.ClusterID,
		ExternalClusterName:     job.ClusterName,
		RawStatus:               job.Status,
		RawStatusMessage:        job.Message,
		NormalizedStatus:        NormalizeStatus(job.Status),
		SubmittedFromWorkingDir: workingDir,
		SubmittedFromProject:    project,
		DashboardURL:            job.Dashboard,
		Command:                 job.Command,
		Description:             descriptionForJob(job),
		SubmittedAt:             job.SubmittedAt,
		StartedAt:               job.StartedAt,
		EndedAt:                 job.EndedAt,
	}
}

func descriptionForJob(job Job) string {
	if job.Name != "" {
		return job.Name
	}
	if job.Command != "" {
		return job.Command
	}
	if job.ID != "" {
		return "SkyPilot job " + job.ID
	}
	return "SkyPilot job"
}

func NormalizeStatus(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	switch s {
	case "", "pending", "submitted", "starting", "provisioning", "recovering", "init", "initializing", "setting_up":
		return db.StatusQueued
	case "running", "active", "winding_down":
		// WINDING_DOWN is still processing: the controller waits for worker
		// threads and merges outputs before it reports a terminal result.
		return db.StatusRunning
	case "succeeded", "success", "completed", "done":
		return db.StatusCompleted
	case "failed", "fail", "error", "errored":
		return db.StatusFailed
	case "cancelling", "canceling":
		// SkyPilot still owns live work while cancellation is in progress.
		// Keep watches active and do not stamp a terminal execution boundary.
		return db.StatusRunning
	case "cancelled", "canceled":
		return db.StatusCanceled
	case "stopped", "killed":
		return db.StatusKilled
	default:
		if strings.Contains(s, "fail") || strings.Contains(s, "error") {
			return db.StatusFailed
		}
		if strings.Contains(s, "cancell") && strings.Contains(s, "ing") {
			return db.StatusRunning
		}
		if strings.Contains(s, "cancel") {
			return db.StatusCanceled
		}
		if strings.Contains(s, "complete") || strings.Contains(s, "success") {
			return db.StatusCompleted
		}
		return db.StatusQueued
	}
}

var submittedJobIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bmanaged\s+job(?:\s+id)?\s*[:=#]?\s*['"]?([0-9]+)\b`),
	regexp.MustCompile(`(?i)\bjob\s+id\s*[:=#]?\s*['"]?([0-9]+)\b`),
}

func submittedJobID(s string) string {
	for _, pattern := range submittedJobIDPatterns {
		match := pattern.FindStringSubmatch(s)
		if len(match) == 2 {
			return match[1]
		}
	}
	return ""
}

func DefaultWorkDir() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return filepath.Clean(".")
}
