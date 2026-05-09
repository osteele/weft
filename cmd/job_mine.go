package cmd

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

var (
	jobMineRecent int
	jobMineJSON   bool
)

var jobAnomaliesCmd = &cobra.Command{
	Use:   "anomalies",
	Short: "List recent jobs that look worth reviewing",
	Long: `List recent jobs that look worth reviewing.

The command looks for failed/dead/canceled jobs, non-zero exits, retries,
multiple attempts, cloud orphan/cancel outcomes, and known metadata patterns
that often explain wasted debugging time.`,
	RunE: runJobAnomalies,
}

var jobChurnCmd = &cobra.Command{
	Use:   "churn",
	Short: "Group recent retry/churn clusters",
	Long: `Group recent jobs that appear to be iterations of the same underlying
experiment or command. Use this to find sequences where job metadata or source
changes should be compared with 'weft job diff' or 'weft source diff'.`,
	RunE: runJobChurn,
}

var jobRecommendCmd = &cobra.Command{
	Use:   "recommend",
	Short: "Suggest Weft improvements from recent job history",
	Long: `Suggest Weft or workflow improvements from recent job history.

This command mines the same signals as 'job anomalies' and 'job churn' and
turns recurring patterns into follow-up recommendations.`,
	RunE: runJobRecommend,
}

type minedJob struct {
	Record  *normalizedJobRecord `json:"job"`
	Reasons []string             `json:"reasons"`
	Score   int                  `json:"score"`
}

type churnGroup struct {
	Key    string   `json:"key"`
	Jobs   []string `json:"jobs"`
	Why    []string `json:"why,omitempty"`
	Score  int      `json:"score"`
	Sample string   `json:"sample,omitempty"`
}

type recommendation struct {
	Issue      string   `json:"issue"`
	Evidence   []string `json:"evidence,omitempty"`
	NextSteps  []string `json:"next_steps,omitempty"`
	Confidence string   `json:"confidence,omitempty"`
}

func init() {
	for _, cmd := range []*cobra.Command{jobAnomaliesCmd, jobChurnCmd, jobRecommendCmd} {
		cmd.Flags().IntVar(&jobMineRecent, "recent", 100, "Number of recent jobs to inspect")
		cmd.Flags().BoolVar(&jobMineJSON, "json", false, "Print machine-readable JSON")
		jobCmd.AddCommand(cmd)
	}
}

func runJobAnomalies(cmd *cobra.Command, args []string) error {
	recs, err := loadRecentNormalizedJobs(jobMineRecent)
	if err != nil {
		return err
	}
	mined := mineAnomalies(recs)
	if jobMineJSON {
		return writeJSON(cmd.OutOrStdout(), mined)
	}
	printAnomalies(cmd.OutOrStdout(), mined)
	return nil
}

func runJobChurn(cmd *cobra.Command, args []string) error {
	recs, err := loadRecentNormalizedJobs(jobMineRecent)
	if err != nil {
		return err
	}
	groups := mineChurn(recs)
	if jobMineJSON {
		return writeJSON(cmd.OutOrStdout(), groups)
	}
	printChurn(cmd.OutOrStdout(), groups)
	return nil
}

func runJobRecommend(cmd *cobra.Command, args []string) error {
	recs, err := loadRecentNormalizedJobs(jobMineRecent)
	if err != nil {
		return err
	}
	anomalies := mineAnomalies(recs)
	churn := mineChurn(recs)
	recommendations := mineRecommendations(anomalies, churn, recs)
	if jobMineJSON {
		return writeJSON(cmd.OutOrStdout(), recommendations)
	}
	printRecommendations(cmd.OutOrStdout(), recommendations)
	return nil
}

func loadRecentNormalizedJobs(limit int) ([]*normalizedJobRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	database, err := db.OpenForReading()
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	jobs, err := db.ListJobsByStatuses(database, nil, "", "", 0, nil, "")
	if err != nil {
		return nil, err
	}
	slices.SortFunc(jobs, func(a, b *db.Job) int {
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	recs := make([]*normalizedJobRecord, 0, len(jobs))
	for _, job := range jobs {
		attempts, err := db.ListAttempts(database, job.ID)
		if err != nil {
			return nil, err
		}
		recs = append(recs, normalizeJobRecord(job, attempts))
	}
	return recs, nil
}

func mineAnomalies(recs []*normalizedJobRecord) []minedJob {
	var out []minedJob
	for _, rec := range recs {
		reasons := anomalyReasons(rec)
		if len(reasons) == 0 {
			continue
		}
		out = append(out, minedJob{Record: rec, Reasons: reasons, Score: anomalyScore(reasons)})
	}
	slices.SortFunc(out, func(a, b minedJob) int {
		if a.Score != b.Score {
			return b.Score - a.Score
		}
		return strings.Compare(b.Record.ID, a.Record.ID)
	})
	return out
}

func anomalyReasons(rec *normalizedJobRecord) []string {
	var reasons []string
	if rec.Status == db.StatusFailed || rec.Status == db.StatusDead || rec.Status == db.StatusCanceled || rec.Status == db.StatusKilled {
		reasons = append(reasons, "terminal status "+rec.Status)
	}
	if rec.ExitCode != nil && *rec.ExitCode != 0 {
		reasons = append(reasons, fmt.Sprintf("exit code %d", *rec.ExitCode))
	}
	if rec.RetryCount > 0 {
		reasons = append(reasons, pluralCount(rec.RetryCount, "retry", "retries"))
	}
	if len(rec.Attempts) > 1 {
		reasons = append(reasons, pluralCount(len(rec.Attempts), "attempt", "attempts"))
	}
	if rec.FailureReason != "" {
		reasons = append(reasons, "failure reason: "+rec.FailureReason)
	}
	for _, attempt := range rec.Attempts {
		if attempt.CloudOutcome == "orphaned" || attempt.CloudOutcome == "canceled" || attempt.CloudOutcome == "preempted" {
			reasons = append(reasons, "cloud outcome "+attempt.CloudOutcome)
			break
		}
	}
	if hasHFInputs(rec) && !hasEnvAssignment(rec.Env, "HF_HUB_OFFLINE") {
		reasons = append(reasons, "HF inputs without explicit HF_HUB_OFFLINE override")
	}
	if hasHFInputs(rec) && !hasEnvAssignment(rec.Env, "HF_HOME") {
		reasons = append(reasons, "HF inputs without explicit HF_HOME")
	}
	if strings.Contains(strings.ToLower(rec.ErrorMessage+" "+rec.FailureReason), "permission") {
		reasons = append(reasons, "permission-related failure")
	}
	return dedupStrings(reasons)
}

func anomalyScore(reasons []string) int {
	score := 0
	for _, reason := range reasons {
		switch {
		case strings.Contains(reason, "attempts"):
			score += 4
		case strings.Contains(reason, "retries"):
			score += 4
		case strings.Contains(reason, "terminal status"):
			score += 3
		case strings.Contains(reason, "exit code"):
			score += 3
		case strings.Contains(reason, "cloud outcome"):
			score += 3
		default:
			score++
		}
	}
	return score
}

func mineChurn(recs []*normalizedJobRecord) []churnGroup {
	groups := map[string][]*normalizedJobRecord{}
	for _, rec := range recs {
		key := churnKey(rec)
		if key == "" {
			continue
		}
		groups[key] = append(groups[key], rec)
	}
	var out []churnGroup
	for key, jobs := range groups {
		if len(jobs) < 3 {
			continue
		}
		slices.SortFunc(jobs, func(a, b *normalizedJobRecord) int {
			return strings.Compare(a.ID, b.ID)
		})
		ids := make([]string, 0, len(jobs))
		for _, job := range jobs {
			ids = append(ids, job.ID)
		}
		why := churnReasons(jobs)
		out = append(out, churnGroup{
			Key:    key,
			Jobs:   ids,
			Why:    why,
			Score:  len(jobs)*2 + len(why),
			Sample: jobs[len(jobs)-1].Command,
		})
	}
	slices.SortFunc(out, func(a, b churnGroup) int {
		if a.Score != b.Score {
			return b.Score - a.Score
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out
}

func churnKey(rec *normalizedJobRecord) string {
	scripts := inferCommandSourcePaths(rec.Command)
	script := ""
	if len(scripts) > 0 {
		script = scripts[0]
	}
	if script == "" {
		script = normalizeChurnCommand(rec.Command)
	}
	project := rec.Project
	if project == "" {
		project = rec.WorkingDir
	}
	if project == "" || script == "" {
		return ""
	}
	return project + " | " + script
}

func churnReasons(jobs []*normalizedJobRecord) []string {
	var reasons []string
	if fieldVaries(jobs, func(j *normalizedJobRecord) interface{} { return j.Env }) {
		reasons = append(reasons, "env changed")
	}
	if fieldVaries(jobs, func(j *normalizedJobRecord) interface{} { return j.Inputs }) {
		reasons = append(reasons, "inputs changed")
	}
	if fieldVaries(jobs, func(j *normalizedJobRecord) interface{} { return j.Host }) {
		reasons = append(reasons, "host changed")
	}
	if fieldVaries(jobs, func(j *normalizedJobRecord) interface{} { return j.GPUClass }) {
		reasons = append(reasons, "GPU class changed")
	}
	if fieldVaries(jobs, func(j *normalizedJobRecord) interface{} { return j.Command }) {
		reasons = append(reasons, "command changed")
	}
	return reasons
}

func mineRecommendations(anomalies []minedJob, churn []churnGroup, recs []*normalizedJobRecord) []recommendation {
	var out []recommendation
	hfNoOffline := jobsWithReason(anomalies, "HF inputs without explicit HF_HUB_OFFLINE override")
	if len(hfNoOffline) > 0 {
		out = append(out, recommendation{
			Issue:      "HF jobs often lack explicit offline-mode overrides",
			Evidence:   limitStrings(hfNoOffline, 5),
			NextSteps:  []string{"Use --env HF_HUB_OFFLINE=0 for jobs that call Hugging Face APIs at runtime.", "Consider documenting host-level HF offline defaults for affected projects."},
			Confidence: "medium",
		})
	}
	hfNoHome := jobsWithReason(anomalies, "HF inputs without explicit HF_HOME")
	if len(hfNoHome) > 0 {
		out = append(out, recommendation{
			Issue:      "HF jobs often rely on the shared default cache",
			Evidence:   limitStrings(hfNoHome, 5),
			NextSteps:  []string{"Use a project-local HF_HOME when NAS UID mismatch or cache permissions are suspected.", "Compare adjacent attempts with weft job diff to confirm the cache override."},
			Confidence: "medium",
		})
	}
	if len(churn) > 0 {
		var evidence []string
		for _, group := range churn {
			if len(group.Jobs) >= 2 {
				evidence = append(evidence, fmt.Sprintf("%s (%d jobs)", group.Key, len(group.Jobs)))
			}
		}
		out = append(out, recommendation{
			Issue:      "High-churn job clusters are available for postmortem comparison",
			Evidence:   limitStrings(evidence, 5),
			NextSteps:  []string{"Run weft job diff on adjacent jobs in the cluster.", "Run weft source diff when metadata changes do not explain behavior."},
			Confidence: "high",
		})
	}
	multiAttempt := jobsWithReason(anomalies, "attempts")
	if len(multiAttempt) > 0 {
		out = append(out, recommendation{
			Issue:      "Some jobs required multiple attempts",
			Evidence:   limitStrings(multiAttempt, 5),
			NextSteps:  []string{"Inspect attempt outcomes with weft job inspect --json.", "Look for repeated cloud orphan, preempted, or canceled outcomes."},
			Confidence: "medium",
		})
	}
	if len(out) == 0 && len(recs) > 0 {
		out = append(out, recommendation{
			Issue:      "No strong recurring pattern found in the selected recent jobs",
			NextSteps:  []string{"Increase --recent or inspect a known high-churn range with weft job churn."},
			Confidence: "low",
		})
	}
	return out
}

func printAnomalies(w io.Writer, mined []minedJob) {
	if len(mined) == 0 {
		fmt.Fprintln(w, "No notable anomalies found.")
		return
	}
	for _, item := range mined {
		fmt.Fprintf(w, "%s  score=%d  status=%s", item.Record.ID, item.Score, item.Record.Status)
		if item.Record.Host != "" {
			fmt.Fprintf(w, "  host=%s", item.Record.Host)
		}
		fmt.Fprintln(w)
		for _, reason := range item.Reasons {
			fmt.Fprintf(w, "  - %s\n", reason)
		}
		fmt.Fprintf(w, "  inspect: weft job inspect %s\n", item.Record.ID)
	}
}

func printChurn(w io.Writer, groups []churnGroup) {
	if len(groups) == 0 {
		fmt.Fprintln(w, "No high-churn groups found.")
		return
	}
	for _, group := range groups {
		fmt.Fprintf(w, "%s  score=%d  jobs=%s\n", group.Key, group.Score, strings.Join(group.Jobs, ","))
		for _, reason := range group.Why {
			fmt.Fprintf(w, "  - %s\n", reason)
		}
		if len(group.Jobs) >= 2 {
			fmt.Fprintf(w, "  diff: weft job diff %s %s\n", group.Jobs[len(group.Jobs)-2], group.Jobs[len(group.Jobs)-1])
			fmt.Fprintf(w, "  source: weft source diff %s %s\n", group.Jobs[len(group.Jobs)-2], group.Jobs[len(group.Jobs)-1])
		}
	}
}

func printRecommendations(w io.Writer, recs []recommendation) {
	for i, rec := range recs {
		fmt.Fprintf(w, "%d. %s", i+1, rec.Issue)
		if rec.Confidence != "" {
			fmt.Fprintf(w, " (%s confidence)", rec.Confidence)
		}
		fmt.Fprintln(w)
		for _, evidence := range rec.Evidence {
			fmt.Fprintf(w, "  evidence: %s\n", evidence)
		}
		for _, step := range rec.NextSteps {
			fmt.Fprintf(w, "  next: %s\n", step)
		}
	}
}

func jobsWithReason(anomalies []minedJob, needle string) []string {
	var out []string
	for _, item := range anomalies {
		for _, reason := range item.Reasons {
			if strings.Contains(reason, needle) {
				out = append(out, item.Record.ID)
				break
			}
		}
	}
	return out
}

func hasHFInputs(rec *normalizedJobRecord) bool {
	for _, input := range rec.Inputs {
		if strings.HasPrefix(input, "hf:") || strings.HasPrefix(input, "hf-dataset:") {
			return true
		}
	}
	return false
}

func fieldVaries(jobs []*normalizedJobRecord, value func(*normalizedJobRecord) interface{}) bool {
	if len(jobs) < 2 {
		return false
	}
	first := formatDiffValue(value(jobs[0]))
	for _, job := range jobs[1:] {
		if formatDiffValue(value(job)) != first {
			return true
		}
	}
	return false
}

var churnNumberRe = regexp.MustCompile(`\b\d+(\.\d+)?\b`)

func normalizeChurnCommand(command string) string {
	command = strings.TrimSpace(command)
	command = churnNumberRe.ReplaceAllString(command, "#")
	return strings.Join(strings.Fields(command), " ")
}

func dedupStrings(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func limitStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

func pluralCount(count int, singular, plural string) string {
	word := plural
	if count == 1 {
		word = singular
	}
	return fmt.Sprintf("%d %s", count, word)
}
