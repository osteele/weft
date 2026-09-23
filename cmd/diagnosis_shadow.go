package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/remediation"
	"github.com/osteele/weft/internal/typesafe"
)

// The diagnosis shadow asks TypeSafe's Jev model to classify each recently
// failed job and records the answer next to weft's three regex diagnoses
// (display, remediation, runtime-fatal) so the two can be compared over real
// traffic. It is a measurement pilot: nothing reads its rows to kill, retry,
// cordon, rediagnose, or display anything.

const (
	diagnosisShadowCallTimeout  = 20 * time.Second
	diagnosisShadowCommandChars = 600
	diagnosisShadowLogTailChars = 4000

	diagnosisShadowCategoryQuestion = "category"
	diagnosisShadowRegexQuestion    = "regex_supported"
	diagnosisShadowFatalQuestion    = "fatal_supported"

	diagnosisShadowInstructions = "Why did the job described in `command` fail, judging from `log_tail` and `exit_code`? Classify the root cause shown in the log, not messages that merely mention an error."
)

type diagnosisShadowCriterion struct {
	What     string   `json:"what"`
	NotFor   string   `json:"not_for"`
	Examples []string `json:"examples"`
}

// diagnosisShadowCategories is the Choice option set sent to Jev.
var diagnosisShadowCategories = map[string]diagnosisShadowCriterion{
	"gpu_oom": {
		What:     "The GPU ran out of memory (CUDA out of memory, torch.OutOfMemoryError)",
		NotFor:   "Host RAM exhaustion or other CUDA errors",
		Examples: []string{"torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB"},
	},
	"cuda_runtime": {
		What:     "A CUDA/driver/GPU runtime error other than out-of-memory: no kernel image for the device, driver too old, device-side assert, NCCL, GPU not visible",
		NotFor:   "GPU out of memory; errors that merely mention CUDA in harness narration",
		Examples: []string{"CUDA error: no kernel image is available for execution on the device"},
	},
	"missing_input": {
		What:     "A file, directory, dataset, model, or checkpoint the job needed was not found",
		NotFor:   "A Python module that cannot be imported",
		Examples: []string{"FileNotFoundError: [Errno 2] No such file or directory: 'output/x.json'"},
	},
	"import_error": {
		What:     "A Python module or name could not be imported",
		NotFor:   "Package installation or resolution failures before the script starts",
		Examples: []string{"ModuleNotFoundError: No module named 'vllm'"},
	},
	"env_setup": {
		What:     "Environment setup failed before the job's own code ran: dependency resolution or installation, uv/pip errors, Python version mismatch, venv creation",
		NotFor:   "Errors raised by the job's own code after it started",
		Examples: []string{"No solution found when resolving dependencies"},
	},
	"timeout": {
		What:     "The job was stopped for exceeding a time limit or hang watchdog (wall time, stdout silence, GPU idle)",
		NotFor:   "Network timeouts inside the job's own code that raised an exception",
		Examples: []string{"Execution timed out after 7200s"},
	},
	"deliberate_check_failure": {
		What:     "The job's own check, test, assertion, or gate failed on purpose: an assert, a failed test suite, a script exiting non-zero because a validation did not pass",
		NotFor:   "Unexpected crashes",
		Examples: []string{"AssertionError: accuracy below threshold", "FAILED tests/test_x.py::test_y"},
	},
	"code_exception": {
		What:     "An unexpected exception or crash in the job's own code not covered by another category",
		NotFor:   "Missing files, imports, GPU errors, or deliberate check failures",
		Examples: []string{"KeyError: 'logits'"},
	},
	"network_transfer": {
		What:     "A network or remote-storage operation failed during the job: download/upload errors, SSH/rsync failures, API rate limits or HTTP errors",
		NotFor:   "Dependency installation failures during environment setup",
		Examples: []string{"429 Too Many Requests"},
	},
	"killed_or_cancelled": {
		What:     "The process was killed by a signal, the host OOM killer, or an operator cancel, without an error of its own",
		NotFor:   "Watchdog time limits",
		Examples: []string{"Killed", "exit=137"},
	},
	"weft_infrastructure": {
		What:     "Weft's own machinery failed: source sync or extraction, preflight provenance, payload staging, agent or runner errors",
		NotFor:   "Failures in the job's own code or environment",
		Examples: []string{"weft: payload staging unavailable"},
	},
	"no_failure_evident": {
		What:     "The log tail shows no failure, or is too truncated to tell why the job failed",
		NotFor:   "Any log with a visible error",
		Examples: []string{"log ends mid-progress bar with no error"},
	},
}

// diagnosisShadowSystemOne sends one judgment request. Tests replace it.
var diagnosisShadowSystemOne = func(ctx context.Context, client *typesafe.Client, req typesafe.Request) (typesafe.Response, error) {
	return client.SystemOne(ctx, req)
}

type diagnosisShadowState struct {
	Command  string `json:"command"`
	ExitCode *int   `json:"exit_code"`
	LogTail  string `json:"log_tail"`
}

// diagnosisShadowPassResult summarizes one pass. RateLimited ends a pass
// early; RetryAfter is the server's requested backoff, when it gave one.
type diagnosisShadowPassResult struct {
	Judged      int
	Failed      int
	NoLog       int
	RateLimited bool
	RetryAfter  time.Duration
}

// runDiagnosisShadow is the daemon worker. It stays off, with one warning,
// when the settings are invalid or no TypeSafe API key is available.
func runDiagnosisShadow(ctx context.Context, database *sql.DB, cfg *config.Config) {
	settings, err := cfg.DiagnosisShadow.Settings()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: diagnosis shadow disabled: %s\n", err)
		return
	}
	key := typesafe.LookupAPIKey()
	if key == "" {
		fmt.Fprintf(os.Stderr, "warning: diagnosis shadow disabled: no TypeSafe API key; set %s in the daemon's environment or add a %s= line to ~/.config/weft/config\n",
			typesafe.APIKeyEnv, typesafe.APIKeyEnv)
		return
	}
	client := typesafe.NewClient(key)
	fmt.Fprintf(os.Stderr, "diagnosis shadow enabled: model=%s interval=%s lookback=%s max_per_pass=%d\n",
		settings.Model, settings.Interval, settings.Lookback, settings.MaxPerPass)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		result := runDiagnosisShadowPass(ctx, database, client, settings)
		wait := settings.Interval
		if result.RetryAfter > wait {
			wait = result.RetryAfter
		}
		timer.Reset(wait)
	}
}

// runDiagnosisShadowPass judges up to settings.MaxPerPass unjudged failed jobs,
// newest first. Jobs without a cached log are skipped without spending a call
// and stay candidates, since a log may be cached later. A failed call stores
// nothing, so the job is retried on a later pass; a 429 ends the pass.
func runDiagnosisShadowPass(ctx context.Context, database *sql.DB, client *typesafe.Client, settings config.DiagnosisShadowSettings) diagnosisShadowPassResult {
	var result diagnosisShadowPassResult
	candidates, err := db.ListDiagnosisShadowCandidates(database, time.Now().Add(-settings.Lookback), 0)
	if err != nil {
		slog.Warn("diagnosis shadow: list candidates failed", "component", "diagnosis_shadow", "error", err)
		return result
	}
	calls := 0
	for _, job := range candidates {
		if calls >= settings.MaxPerPass || ctx.Err() != nil {
			break
		}
		logContent, err := logcache.Read(job.ID)
		switch {
		case errors.Is(err, fs.ErrNotExist) || (err == nil && strings.TrimSpace(logContent) == ""):
			slog.Debug("diagnosis shadow: no cached log; skipping this pass", "component", "diagnosis_shadow", "job_id", job.ID)
			result.NoLog++
			continue
		case err != nil:
			slog.Warn("diagnosis shadow: read cached log failed; skipping this pass", "component", "diagnosis_shadow", "job_id", job.ID, "error", err)
			result.Failed++
			continue
		}
		calls++
		row, err := judgeDiagnosisShadow(ctx, client, settings.Model, job, logContent)
		if err != nil {
			result.Failed++
			slog.Warn("diagnosis shadow: judgment failed; will retry on a later pass", "component", "diagnosis_shadow", "job_id", job.ID, "error", err)
			var apiErr *typesafe.APIError
			if errors.As(err, &apiErr) && apiErr.RateLimited() {
				result.RateLimited = true
				result.RetryAfter = apiErr.RetryAfter
				break
			}
			continue
		}
		if err := db.InsertDiagnosisShadow(database, row); err != nil {
			result.Failed++
			slog.Warn("diagnosis shadow: store judgment failed; will retry on a later pass", "component", "diagnosis_shadow", "job_id", job.ID, "error", err)
			continue
		}
		result.Judged++
		oplog.LogJob(oplog.OpDiagnosisShadow, job.ID, job.Host,
			oplog.WithDetail(formatDiagnosisShadowDetail(row)),
			oplog.WithDuration(time.Duration(row.LatencyMS)*time.Millisecond))
	}
	return result
}

// judgeDiagnosisShadow computes weft's three regex diagnoses of logContent and
// asks Jev, in one request, for its category plus whether the log supports
// each regex diagnosis that exists.
func judgeDiagnosisShadow(ctx context.Context, client *typesafe.Client, model string, job *db.Job, logContent string) (db.DiagnosisShadow, error) {
	display := remediation.ResolveDisplayDiagnosis(job, func() (string, error) { return logContent, nil })
	remediationDiag := remediation.DiagnoseFailedAttemptFromLog(logContent, "shadow")
	fatal := remediation.CheckFatalAtRuntime(logContent)

	criteria := make(map[string]any, len(diagnosisShadowCategories))
	for key, c := range diagnosisShadowCategories {
		criteria[key] = c
	}
	req := typesafe.Request{
		Model: model,
		State: diagnosisShadowState{
			Command:  headChars(job.Command, diagnosisShadowCommandChars),
			ExitCode: job.ExitCode,
			LogTail:  tailChars(logContent, diagnosisShadowLogTailChars),
		},
		Choices: map[string]typesafe.ChoiceQuestion{
			diagnosisShadowCategoryQuestion: {Instructions: diagnosisShadowInstructions, Criteria: criteria},
		},
	}
	// The regex claim under test is the one weft displays, falling back to
	// the remediation path's; "unknown" is that path's no-match marker.
	regexClaim := display
	if regexClaim == nil && remediationDiag != nil && remediationDiag.Pattern != "unknown" {
		regexClaim = remediationDiag
	}
	nouls := map[string]typesafe.NoulQuestion{}
	if regexClaim != nil {
		nouls[diagnosisShadowRegexQuestion] = typesafe.NoulQuestion{Instructions: diagnosisShadowSupportQuestion(regexClaim)}
	}
	if fatal != nil {
		nouls[diagnosisShadowFatalQuestion] = typesafe.NoulQuestion{Instructions: diagnosisShadowSupportQuestion(fatal)}
	}
	if len(nouls) > 0 {
		req.Nouls = nouls
	}

	callCtx, cancel := context.WithTimeout(ctx, diagnosisShadowCallTimeout)
	defer cancel()
	started := time.Now()
	resp, err := diagnosisShadowSystemOne(callCtx, client, req)
	latency := time.Since(started)
	if err != nil {
		return db.DiagnosisShadow{}, err
	}
	answer, ok := resp.Choices[diagnosisShadowCategoryQuestion]
	if !ok {
		return db.DiagnosisShadow{}, fmt.Errorf("response has no %q answer", diagnosisShadowCategoryQuestion)
	}
	probabilities, err := json.Marshal(answer.Probabilities)
	if err != nil {
		return db.DiagnosisShadow{}, fmt.Errorf("encode probabilities: %w", err)
	}
	respModel := resp.Model
	if respModel == "" {
		respModel = model
	}
	row := db.DiagnosisShadow{
		JobID:              job.ID,
		AttemptID:          job.LatestRunID,
		JudgedAt:           time.Now().Unix(),
		Model:              respModel,
		DisplayPattern:     diagnosisPattern(display),
		RemediationPattern: diagnosisPattern(remediationDiag),
		FatalPattern:       diagnosisPattern(fatal),
		JevCategory:        answer.Choice,
		JevConfidence:      answer.Confidence,
		JevProbabilities:   probabilities,
		LatencyMS:          latency.Milliseconds(),
		InputTokens:        resp.Usage.InputTokens,
	}
	if _, asked := nouls[diagnosisShadowRegexQuestion]; asked {
		v, ok := resp.Nouls[diagnosisShadowRegexQuestion]
		if !ok {
			return db.DiagnosisShadow{}, fmt.Errorf("response has no %q answer", diagnosisShadowRegexQuestion)
		}
		row.RegexSupported = &v.Noul
	}
	if _, asked := nouls[diagnosisShadowFatalQuestion]; asked {
		v, ok := resp.Nouls[diagnosisShadowFatalQuestion]
		if !ok {
			return db.DiagnosisShadow{}, fmt.Errorf("response has no %q answer", diagnosisShadowFatalQuestion)
		}
		row.FatalSupported = &v.Noul
	}
	return row, nil
}

func diagnosisShadowSupportQuestion(d *remediation.ErrorDiagnosis) string {
	claim := strings.TrimSpace(d.Message)
	if claim == "" {
		claim = d.Pattern
	}
	return "Does `log_tail` show that the job failed because " + claim + "?"
}

func diagnosisPattern(d *remediation.ErrorDiagnosis) string {
	if d == nil {
		return ""
	}
	return d.Pattern
}

func formatDiagnosisShadowDetail(row db.DiagnosisShadow) string {
	return fmt.Sprintf("display=%s remediation=%s fatal=%s jev=%s confidence=%.2f regex_supported=%s fatal_supported=%s model=%s latency_ms=%d",
		dashIfEmpty(row.DisplayPattern), dashIfEmpty(row.RemediationPattern), dashIfEmpty(row.FatalPattern),
		row.JevCategory, row.JevConfidence, formatOptionalNoul(row.RegexSupported), formatOptionalNoul(row.FatalSupported),
		row.Model, row.LatencyMS)
}

func formatOptionalNoul(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *v)
}

// headChars returns the first n characters (runes) of s.
func headChars(s string, n int) string {
	i := 0
	for count := 0; i < len(s) && count < n; count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// tailChars returns the last n characters (runes) of s.
func tailChars(s string, n int) string {
	i := len(s)
	for count := 0; i > 0 && count < n; count++ {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}
