package skypilot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/db"
)

type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
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

type Client struct {
	Runner Runner
}

type Job struct {
	ID          string
	TaskID      string
	Name        string
	Status      string
	Message     string
	ClusterID   string
	ClusterName string
	Command     string
	Dashboard   string
}

func (c Client) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return CLIRunner{}
}

func (c Client) ListJobs(ctx context.Context) ([]Job, error) {
	out, err := c.runner().Run(ctx, "jobs", "queue", "--output", "json")
	if err != nil {
		return nil, err
	}
	return ParseJobsQueueJSON(out)
}

func (c Client) Logs(ctx context.Context, externalJobID string, follow bool) ([]byte, error) {
	args := []string{"jobs", "logs", externalJobID}
	if follow {
		args = append(args, "--follow")
	}
	return c.runner().Run(ctx, args...)
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

	out, err := c.runner().Run(ctx, "jobs", "launch", "-y", "--name", opts.Name, taskPath)
	if err != nil {
		return nil, taskPath, err
	}
	jobs, listErr := c.ListJobs(ctx)
	if listErr == nil {
		if job := FindJobByName(jobs, opts.Name); job != nil {
			return job, taskPath, nil
		}
	}
	if id := firstIntegerToken(string(out)); id != "" {
		return &Job{ID: id, Name: opts.Name, Status: "submitted"}, taskPath, nil
	}
	if listErr != nil {
		return nil, taskPath, fmt.Errorf("submitted SkyPilot task but could not resolve external job id: %w", listErr)
	}
	return nil, taskPath, fmt.Errorf("submitted SkyPilot task but could not resolve external job id")
}

func BuildTaskYAML(opts SubmitOptions) (string, error) {
	if strings.TrimSpace(opts.Command) == "" {
		return "", fmt.Errorf("command is required")
	}
	var b strings.Builder
	if opts.Name != "" {
		fmt.Fprintf(&b, "name: %s\n", quoteYAML(opts.Name))
	}
	if opts.WorkDir != "" {
		fmt.Fprintf(&b, "workdir: %s\n", quoteYAML(opts.WorkDir))
	}
	if opts.GPUClass != "" || opts.GPUMemGB != nil {
		b.WriteString("resources:\n")
		if opts.GPUClass != "" {
			count := opts.GPUCount
			if count <= 0 {
				count = 1
			}
			fmt.Fprintf(&b, "  accelerators: %s:%d\n", strings.ToUpper(opts.GPUClass), count)
		}
		if opts.GPUMemGB != nil {
			fmt.Fprintf(&b, "  accelerator_args:\n    gpu_memory: %d\n", *opts.GPUMemGB)
		}
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
		TaskID:      firstString(m, "task_id", "task", "task_name"),
		Name:        firstString(m, "name", "job_name", "task_name"),
		Status:      firstString(m, "status", "state", "job_status"),
		Message:     firstString(m, "status_message", "message", "details", "failure_reason"),
		ClusterID:   firstString(m, "cluster_id"),
		ClusterName: firstString(m, "cluster", "cluster_name", "cluster_id"),
		Command:     firstString(m, "command", "run", "run_command"),
		Dashboard:   firstString(m, "dashboard_url", "url"),
	}
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

func FindJob(jobs []Job, externalID string) *Job {
	externalID = strings.TrimSpace(externalID)
	for i := range jobs {
		if jobs[i].ID == externalID || jobs[i].TaskID == externalID || jobs[i].Name == externalID {
			return &jobs[i]
		}
	}
	return nil
}

func FindJobByName(jobs []Job, name string) *Job {
	name = strings.TrimSpace(name)
	for i := range jobs {
		if jobs[i].Name == name {
			return &jobs[i]
		}
	}
	return nil
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
	case "running", "active":
		return db.StatusRunning
	case "succeeded", "success", "completed", "done":
		return db.StatusCompleted
	case "failed", "fail", "error", "errored":
		return db.StatusFailed
	case "cancelling", "canceling", "cancelled", "canceled":
		return db.StatusCanceled
	case "stopped", "killed":
		return db.StatusKilled
	default:
		if strings.Contains(s, "fail") || strings.Contains(s, "error") {
			return db.StatusFailed
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

var integerTokenRE = regexp.MustCompile(`\b[0-9]+\b`)

func firstIntegerToken(s string) string {
	return integerTokenRE.FindString(s)
}

func DefaultWorkDir() string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return filepath.Clean(".")
}
