package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

var (
	jobInspectJSON bool
	jobDiffJSON    bool
)

var jobInspectCmd = &cobra.Command{
	Use:   "inspect <job-id>",
	Short: "Print normalized job metadata",
	Long: `Print normalized job metadata for comparison and postmortem analysis.

Use --json for a stable machine-readable record.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runJobInspect,
}

var jobDiffCmd = &cobra.Command{
	Use:   "diff <job-a> <job-b>",
	Short: "Compare job metadata and attempts",
	Long: `Compare job metadata and attempts.

This focuses on behaviorally meaningful submission and execution fields:
command, directory, project, host/launch target, inputs, outputs, constraints,
environment, tags, placement metadata, and attempt outcomes.`,
	Args: usageArgs(cobra.ExactArgs(2)),
	RunE: runJobDiff,
}

func init() {
	jobInspectCmd.Flags().BoolVar(&jobInspectJSON, "json", false, "Print normalized metadata as JSON")
	jobDiffCmd.Flags().BoolVar(&jobDiffJSON, "json", false, "Print diff as JSON")
	jobCmd.AddCommand(jobInspectCmd)
	jobCmd.AddCommand(jobDiffCmd)
}

type normalizedJobRecord struct {
	ID                   string                      `json:"id"`
	Status               string                      `json:"status,omitempty"`
	Host                 string                      `json:"host,omitempty"`
	LaunchID             string                      `json:"launch_id,omitempty"`
	Project              string                      `json:"project,omitempty"`
	WorkingDir           string                      `json:"working_dir,omitempty"`
	Command              string                      `json:"command,omitempty"`
	Description          string                      `json:"description,omitempty"`
	Backend              string                      `json:"backend,omitempty"`
	RemoteID             string                      `json:"remote_id,omitempty"`
	RemoteState          string                      `json:"remote_state,omitempty"`
	FailureReason        string                      `json:"failure_reason,omitempty"`
	ErrorMessage         string                      `json:"error_message,omitempty"`
	GPU                  string                      `json:"gpu,omitempty"`
	GPUClass             string                      `json:"gpu_class,omitempty"`
	GPUMemGB             *int                        `json:"gpu_mem_gb,omitempty"`
	GPUMemMaxGB          *int                        `json:"gpu_mem_max_gb,omitempty"`
	MaxComputeCap        string                      `json:"max_compute_cap,omitempty"`
	CPUAllotment         *int                        `json:"cpu_allotment,omitempty"`
	Priority             int                         `json:"priority,omitempty"`
	Env                  []string                    `json:"env,omitempty"`
	Tags                 []string                    `json:"tags,omitempty"`
	Inputs               []string                    `json:"inputs,omitempty"`
	ObservedInputs       []string                    `json:"observed_inputs,omitempty"`
	Outputs              []string                    `json:"outputs,omitempty"`
	OutputDirs           []string                    `json:"output_dirs,omitempty"`
	Produces             []string                    `json:"produces,omitempty"`
	Needs                []string                    `json:"needs,omitempty"`
	DepSpec              string                      `json:"dep_spec,omitempty"`
	PlacementReasons     []string                    `json:"placement_reasons,omitempty"`
	PlacementMeta        *db.PlacementMeta           `json:"placement_meta,omitempty"`
	CLIResourceOverrides *db.CLIResourceOverrides    `json:"cli_overrides,omitempty"`
	CreatedAt            int64                       `json:"created_at,omitempty"`
	QueuedAt             int64                       `json:"queued_at,omitempty"`
	StartTime            int64                       `json:"start_time,omitempty"`
	EndTime              *int64                      `json:"end_time,omitempty"`
	ExitCode             *int                        `json:"exit_code,omitempty"`
	Cost                 *float64                    `json:"cost,omitempty"`
	ErrorDiagnosis       string                      `json:"error_diagnosis,omitempty"`
	RetryCount           int                         `json:"retry_count,omitempty"`
	Attempts             []normalizedAttempt         `json:"attempts,omitempty"`
	Publication          *db.AttemptPublicationState `json:"publication,omitempty"`
}

type normalizedAttempt struct {
	ID            int64                       `json:"id"`
	Number        int                         `json:"number"`
	Host          string                      `json:"host,omitempty"`
	LaunchID      string                      `json:"launch_id,omitempty"`
	Status        string                      `json:"status,omitempty"`
	QueuedAt      *int64                      `json:"queued_at,omitempty"`
	StartTime     *int64                      `json:"start_time,omitempty"`
	EndTime       *int64                      `json:"end_time,omitempty"`
	ExitCode      *int                        `json:"exit_code,omitempty"`
	ErrorMessage  string                      `json:"error_message,omitempty"`
	FailureReason string                      `json:"failure_reason,omitempty"`
	CloudOutcome  string                      `json:"cloud_outcome,omitempty"`
	Backend       string                      `json:"backend,omitempty"`
	Publication   *db.AttemptPublicationState `json:"publication,omitempty"`
}

type jobFieldDiff struct {
	Field string      `json:"field"`
	A     interface{} `json:"a,omitempty"`
	B     interface{} `json:"b,omitempty"`
}

type jobDiffRecord struct {
	A       string         `json:"a"`
	B       string         `json:"b"`
	Changes []jobFieldDiff `json:"changes"`
}

func runJobInspect(cmd *cobra.Command, args []string) error {
	jobID, err := parseSingleJobID(args[0])
	if err != nil {
		return err
	}
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	rec, err := loadNormalizedJobRecord(database, jobID)
	if err != nil {
		return err
	}
	if jobInspectJSON {
		return writeJSON(cmd.OutOrStdout(), rec)
	}
	printJobInspect(cmd.OutOrStdout(), rec)
	return nil
}

func runJobDiff(cmd *cobra.Command, args []string) error {
	aID, err := parseSingleJobID(args[0])
	if err != nil {
		return err
	}
	bID, err := parseSingleJobID(args[1])
	if err != nil {
		return err
	}
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	a, err := loadNormalizedJobRecord(database, aID)
	if err != nil {
		return err
	}
	b, err := loadNormalizedJobRecord(database, bID)
	if err != nil {
		return err
	}
	diff := diffNormalizedJobs(a, b)
	if jobDiffJSON {
		return writeJSON(cmd.OutOrStdout(), diff)
	}
	printJobDiff(cmd.OutOrStdout(), diff)
	return nil
}

func loadNormalizedJobRecord(database *sql.DB, jobID int64) (*normalizedJobRecord, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return nil, fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
	}
	if job == nil {
		return nil, fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	attempts, err := db.ListAttempts(database, jobID)
	if err != nil {
		return nil, err
	}
	rec := normalizeJobRecord(job, attempts)
	for i := range rec.Attempts {
		publication, err := db.GetAttemptPublicationState(database, rec.Attempts[i].ID)
		if err != nil {
			return nil, fmt.Errorf("get publication state for attempt %d: %w", rec.Attempts[i].ID, err)
		}
		rec.Attempts[i].Publication = publication
		if job.LatestRunID != nil && rec.Attempts[i].ID == *job.LatestRunID {
			rec.Publication = publication
		}
	}
	return rec, nil
}

func normalizeJobRecord(job *db.Job, attempts []db.JobAttempt) *normalizedJobRecord {
	rec := &normalizedJobRecord{
		ID:                   ids.FormatJobID(job.ID),
		Status:               job.Status,
		Host:                 job.Host,
		Project:              job.Project,
		WorkingDir:           job.WorkingDir,
		Command:              job.Command,
		Description:          firstNonEmpty(job.Description, job.GeneratedDescription),
		Backend:              job.Backend,
		RemoteID:             job.RemoteID,
		RemoteState:          job.RemoteState,
		FailureReason:        job.FailureReason,
		ErrorMessage:         job.ErrorMessage,
		GPU:                  job.GPU,
		GPUClass:             job.GPUClass,
		GPUMemGB:             job.GPUMemGB,
		GPUMemMaxGB:          job.GPUMemMaxGB,
		MaxComputeCap:        job.MaxComputeCap,
		CPUAllotment:         job.CPUAllotment,
		Priority:             job.Priority,
		Env:                  redactEnvVars(job.EnvVars),
		Tags:                 sortedCopy(job.Tags),
		Inputs:               sortedCopy(job.Inputs),
		ObservedInputs:       sortedCopy(job.ObservedInputs),
		Outputs:              sortedCopy(job.Outputs),
		OutputDirs:           sortedCopy(job.OutputDirs),
		Produces:             sortedCopy(job.Produces),
		Needs:                sortedCopy(job.Needs),
		DepSpec:              job.DepSpec,
		PlacementReasons:     sortedCopy(job.PlacementReasons),
		PlacementMeta:        job.PlacementMeta,
		CLIResourceOverrides: job.CLIResourceOverrides,
		CreatedAt:            job.CreatedAt,
		QueuedAt:             job.QueuedAt,
		StartTime:            job.StartTime,
		EndTime:              job.EndTime,
		ExitCode:             job.ExitCode,
		Cost:                 job.Cost,
		ErrorDiagnosis:       job.ErrorDiagnosis,
		RetryCount:           job.RetryCount,
	}
	if job.LaunchID != nil {
		rec.LaunchID = ids.FormatInstanceID(*job.LaunchID)
	}
	for _, attempt := range attempts {
		a := normalizedAttempt{
			ID:            attempt.ID,
			Number:        attempt.AttemptNumber,
			Host:          attempt.Host,
			Status:        attempt.Status,
			QueuedAt:      attempt.QueuedAt,
			StartTime:     attempt.StartTime,
			EndTime:       attempt.EndTime,
			ExitCode:      attempt.ExitCode,
			ErrorMessage:  attempt.ErrorMessage,
			FailureReason: attempt.FailureReason,
			CloudOutcome:  attempt.CloudOutcome,
			Backend:       attempt.Backend,
		}
		if attempt.LaunchID != nil {
			a.LaunchID = ids.FormatInstanceID(*attempt.LaunchID)
		}
		rec.Attempts = append(rec.Attempts, a)
	}
	return rec
}

func diffNormalizedJobs(a, b *normalizedJobRecord) jobDiffRecord {
	fields := []struct {
		name string
		a    interface{}
		b    interface{}
	}{
		{"status", a.Status, b.Status},
		{"host", a.Host, b.Host},
		{"launch_id", a.LaunchID, b.LaunchID},
		{"project", a.Project, b.Project},
		{"working_dir", a.WorkingDir, b.WorkingDir},
		{"command", a.Command, b.Command},
		{"description", a.Description, b.Description},
		{"backend", a.Backend, b.Backend},
		{"remote_id", a.RemoteID, b.RemoteID},
		{"remote_state", a.RemoteState, b.RemoteState},
		{"failure_reason", a.FailureReason, b.FailureReason},
		{"error_message", a.ErrorMessage, b.ErrorMessage},
		{"gpu", a.GPU, b.GPU},
		{"gpu_class", a.GPUClass, b.GPUClass},
		{"gpu_mem_gb", a.GPUMemGB, b.GPUMemGB},
		{"gpu_mem_max_gb", a.GPUMemMaxGB, b.GPUMemMaxGB},
		{"max_compute_cap", a.MaxComputeCap, b.MaxComputeCap},
		{"cpu_allotment", a.CPUAllotment, b.CPUAllotment},
		{"priority", a.Priority, b.Priority},
		{"env", a.Env, b.Env},
		{"tags", a.Tags, b.Tags},
		{"inputs", a.Inputs, b.Inputs},
		{"observed_inputs", a.ObservedInputs, b.ObservedInputs},
		{"outputs", a.Outputs, b.Outputs},
		{"output_dirs", a.OutputDirs, b.OutputDirs},
		{"produces", a.Produces, b.Produces},
		{"needs", a.Needs, b.Needs},
		{"dep_spec", a.DepSpec, b.DepSpec},
		{"placement_reasons", a.PlacementReasons, b.PlacementReasons},
		{"placement_meta", a.PlacementMeta, b.PlacementMeta},
		{"cli_overrides", a.CLIResourceOverrides, b.CLIResourceOverrides},
		{"exit_code", a.ExitCode, b.ExitCode},
		{"cost", a.Cost, b.Cost},
		{"error_diagnosis", a.ErrorDiagnosis, b.ErrorDiagnosis},
		{"retry_count", a.RetryCount, b.RetryCount},
		{"attempts", a.Attempts, b.Attempts},
		{"publication", a.Publication, b.Publication},
	}
	out := jobDiffRecord{A: a.ID, B: b.ID}
	for _, field := range fields {
		if reflect.DeepEqual(field.a, field.b) {
			continue
		}
		out.Changes = append(out.Changes, jobFieldDiff{Field: field.name, A: field.a, B: field.b})
	}
	return out
}

func printJobInspect(w io.Writer, rec *normalizedJobRecord) {
	fmt.Fprintf(w, "%s\n", rec.ID)
	for _, line := range jobInspectLines(rec) {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

func printJobDiff(w io.Writer, diff jobDiffRecord) {
	fmt.Fprintf(w, "%s -> %s\n", diff.A, diff.B)
	if len(diff.Changes) == 0 {
		fmt.Fprintln(w, "  no metadata differences")
		return
	}
	for _, change := range diff.Changes {
		fmt.Fprintf(w, "  %s:\n", change.Field)
		fmt.Fprintf(w, "    %s: %s\n", diff.A, formatDiffValue(change.A))
		fmt.Fprintf(w, "    %s: %s\n", diff.B, formatDiffValue(change.B))
	}
}

func jobInspectLines(rec *normalizedJobRecord) []string {
	var lines []string
	add := func(name string, value interface{}) {
		if isZeroDiffValue(value) {
			return
		}
		lines = append(lines, fmt.Sprintf("%s: %s", name, formatDiffValue(value)))
	}
	add("status", rec.Status)
	add("host", rec.Host)
	add("launch_id", rec.LaunchID)
	add("project", rec.Project)
	add("working_dir", rec.WorkingDir)
	add("command", rec.Command)
	add("description", rec.Description)
	add("gpu", rec.GPU)
	add("gpu_class", rec.GPUClass)
	add("gpu_mem_gb", rec.GPUMemGB)
	add("env", rec.Env)
	add("tags", rec.Tags)
	add("inputs", rec.Inputs)
	add("outputs", rec.Outputs)
	add("needs", rec.Needs)
	add("produces", rec.Produces)
	add("placement_reasons", rec.PlacementReasons)
	add("publication", rec.Publication)
	add("attempts", rec.Attempts)
	return lines
}

func formatDiffValue(value interface{}) string {
	if isZeroDiffValue(value) {
		return "(empty)"
	}
	data, err := marshalJSONNoHTMLEscape(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimSpace(string(data))
}

func isZeroDiffValue(value interface{}) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return v.IsNil()
	case reflect.String, reflect.Slice, reflect.Map:
		return v.Len() == 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Bool:
		return !v.Bool()
	default:
		return false
	}
}

func writeJSON(w io.Writer, value interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}

func marshalJSONNoHTMLEscape(value interface{}) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func sortedCopy(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	slices.Sort(out)
	return out
}

func redactEnvVars(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if !ok {
			out = append(out, value)
			continue
		}
		if shouldRedactEnvKey(key) || strings.HasPrefix(val, "secret:") {
			out = append(out, key+"=<redacted>")
			continue
		}
		out = append(out, value)
	}
	slices.Sort(out)
	return out
}

func shouldRedactEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, token := range []string{"TOKEN", "SECRET", "PASSWORD", "PASS", "KEY", "CREDENTIAL"} {
		if strings.Contains(upper, token) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
