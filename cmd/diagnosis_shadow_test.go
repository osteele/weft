package cmd

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/remediation"
	"github.com/osteele/weft/internal/typesafe"
)

const shadowTestLog = "loading model\nRuntimeError: CUDA error: no kernel image is available for execution on the device\n=== END exit=1 ===\n"

var shadowTestSettings = config.DiagnosisShadowSettings{
	Model:      "jev-1.13.0",
	Interval:   time.Minute,
	Lookback:   14 * 24 * time.Hour,
	MaxPerPass: 20,
}

type shadowFake struct {
	requests []typesafe.Request
	fail     error
}

func (f *shadowFake) systemOne(_ context.Context, _ *typesafe.Client, req typesafe.Request) (typesafe.Response, error) {
	f.requests = append(f.requests, req)
	if f.fail != nil {
		return typesafe.Response{}, f.fail
	}
	resp := typesafe.Response{
		Model: "jev-1.13.0",
		Choices: map[string]typesafe.ChoiceAnswer{
			diagnosisShadowCategoryQuestion: {Choice: "cuda_runtime", Probabilities: map[string]float64{"cuda_runtime": 0.97, "gpu_oom": 0.03}, Confidence: 0.96},
		},
		Nouls: map[string]typesafe.NoulAnswer{},
		Usage: typesafe.Usage{InputTokens: 1234},
	}
	if _, ok := req.Nouls[diagnosisShadowRegexQuestion]; ok {
		resp.Nouls[diagnosisShadowRegexQuestion] = typesafe.NoulAnswer{Noul: 0.1}
	}
	if _, ok := req.Nouls[diagnosisShadowFatalQuestion]; ok {
		resp.Nouls[diagnosisShadowFatalQuestion] = typesafe.NoulAnswer{Noul: 0.9}
	}
	return resp, nil
}

func useShadowFake(t *testing.T, fake *shadowFake) {
	t.Helper()
	orig := diagnosisShadowSystemOne
	diagnosisShadowSystemOne = fake.systemOne
	t.Cleanup(func() { diagnosisShadowSystemOne = orig })
}

func setupShadowTest(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return db.SetupTestDB(t)
}

func recordShadowFailedJob(t *testing.T, database *sql.DB, command, logContent string) int64 {
	t.Helper()
	id, err := db.RecordQueued(database, "hostA", "/tmp", command, "shadow test")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.RecordCompletionByID(database, id, 1, time.Now().Unix()); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}
	if logContent != "" {
		if err := logcache.Write(id, logContent); err != nil {
			t.Fatalf("logcache.Write: %v", err)
		}
	}
	return id
}

func shadowRows(t *testing.T, database *sql.DB) map[int64]db.DiagnosisShadow {
	t.Helper()
	rows, err := db.ListRecentDiagnosisShadows(database, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("ListRecentDiagnosisShadows: %v", err)
	}
	out := make(map[int64]db.DiagnosisShadow, len(rows))
	for _, row := range rows {
		out[row.JobID] = row
	}
	return out
}

func TestDiagnosisShadowPassRecordsJudgmentsWithoutTouchingJobs(t *testing.T) {
	database := setupShadowTest(t)
	fake := &shadowFake{}
	useShadowFake(t, fake)

	judgedID := recordShadowFailedJob(t, database, "python train.py --epochs 3", shadowTestLog)
	// The stored diagnosis is what weft displays, so it must be the display
	// pattern even though the log says otherwise; it must also not exclude
	// the job, and the pass must leave it alone.
	stored, err := remediation.MarshalDiagnosis(&remediation.ErrorDiagnosis{Pattern: "module_not_found", Category: "environment", Message: "missing module: vllm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateErrorDiagnosis(database, judgedID, stored, 0); err != nil {
		t.Fatalf("UpdateErrorDiagnosis: %v", err)
	}
	noLogID := recordShadowFailedJob(t, database, "python eval.py", "")
	priorID := recordShadowFailedJob(t, database, "python prior.py", shadowTestLog)
	prior := db.DiagnosisShadow{JobID: priorID, JudgedAt: time.Now().Add(-time.Hour).Unix(), Model: "jev-1.13.0", JevCategory: "code_exception", JevProbabilities: []byte("{}")}
	if err := db.InsertDiagnosisShadow(database, prior); err != nil {
		t.Fatalf("InsertDiagnosisShadow: %v", err)
	}

	before := map[int64]*db.Job{}
	for _, id := range []int64{judgedID, noLogID, priorID} {
		job, err := db.GetJobByID(database, id)
		if err != nil {
			t.Fatalf("GetJobByID: %v", err)
		}
		before[id] = job
	}

	result := runDiagnosisShadowPass(context.Background(), database, nil, shadowTestSettings)
	if result.Judged != 1 || result.Failed != 0 || result.NoLog != 1 {
		t.Fatalf("pass result = %+v, want 1 judged, 0 failed, 1 without a log", result)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("SystemOne called %d times, want once (no-log and already-judged jobs are not sent)", len(fake.requests))
	}
	req := fake.requests[0]
	state, ok := req.State.(diagnosisShadowState)
	if !ok || state.Command != "python train.py --epochs 3" || state.ExitCode == nil || *state.ExitCode != 1 || state.LogTail != shadowTestLog {
		t.Fatalf("state = %+v, want the judged job's command, exit code, and log tail", req.State)
	}
	if req.Model != "jev-1.13.0" || len(req.Choices[diagnosisShadowCategoryQuestion].Criteria) != 12 {
		t.Fatalf("request model=%q criteria=%d, want pinned model and 12 categories", req.Model, len(req.Choices[diagnosisShadowCategoryQuestion].Criteria))
	}
	regexQ, askedRegex := req.Nouls[diagnosisShadowRegexQuestion]
	if !askedRegex || !strings.Contains(regexQ.Instructions.(string), "missing module: vllm") {
		t.Fatalf("regex_supported question = %+v, want one about the displayed diagnosis", regexQ)
	}
	fatal := remediation.CheckFatalAtRuntime(shadowTestLog)
	if fatal == nil {
		t.Fatal("fixture log should match a runtime-fatal pattern")
	}
	if _, askedFatal := req.Nouls[diagnosisShadowFatalQuestion]; !askedFatal {
		t.Fatal("fatal_supported not asked although a fatal pattern exists")
	}

	rows := shadowRows(t, database)
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want the new judgment plus the prior one", rows)
	}
	row, ok := rows[judgedID]
	if !ok {
		t.Fatalf("no row for judged job %d", judgedID)
	}
	wantRemediation := remediation.DiagnoseFailedAttemptFromLog(shadowTestLog, "shadow").Pattern
	if row.DisplayPattern != "module_not_found" || row.RemediationPattern != wantRemediation || row.FatalPattern != fatal.Pattern {
		t.Errorf("patterns display=%q remediation=%q fatal=%q, want module_not_found/%q/%q",
			row.DisplayPattern, row.RemediationPattern, row.FatalPattern, wantRemediation, fatal.Pattern)
	}
	if row.JevCategory != "cuda_runtime" || row.JevConfidence != 0.96 || !strings.Contains(string(row.JevProbabilities), `"cuda_runtime":0.97`) {
		t.Errorf("jev fields = %q %v %q", row.JevCategory, row.JevConfidence, row.JevProbabilities)
	}
	if row.RegexSupported == nil || *row.RegexSupported != 0.1 || row.FatalSupported == nil || *row.FatalSupported != 0.9 {
		t.Errorf("nouls regex=%v fatal=%v, want 0.1 and 0.9", row.RegexSupported, row.FatalSupported)
	}
	if row.Model != "jev-1.13.0" || row.InputTokens != 1234 || row.AttemptID == nil || row.JudgedAt == 0 {
		t.Errorf("row metadata = %+v", row)
	}
	if _, ok := rows[noLogID]; ok {
		t.Errorf("job %d without a cached log got a row", noLogID)
	}
	if rows[priorID].JevCategory != "code_exception" {
		t.Errorf("prior judgment was overwritten: %+v", rows[priorID])
	}

	for id, want := range before {
		got, err := db.GetJobByID(database, id)
		if err != nil {
			t.Fatalf("GetJobByID: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("job %d changed:\n got  %+v\n want %+v", id, got, want)
		}
	}

	// A second pass has nothing left to send.
	runDiagnosisShadowPass(context.Background(), database, nil, shadowTestSettings)
	if len(fake.requests) != 1 {
		t.Fatalf("second pass re-sent a judged job: %d calls", len(fake.requests))
	}
}

func TestDiagnosisShadowPassAPIErrorStoresNothingAndRetries(t *testing.T) {
	database := setupShadowTest(t)
	fake := &shadowFake{fail: errors.New("connection reset")}
	useShadowFake(t, fake)
	id := recordShadowFailedJob(t, database, "python train.py", shadowTestLog)

	result := runDiagnosisShadowPass(context.Background(), database, nil, shadowTestSettings)
	if result.Failed != 1 || result.Judged != 0 {
		t.Fatalf("failing pass result = %+v, want 1 failed", result)
	}
	if rows := shadowRows(t, database); len(rows) != 0 {
		t.Fatalf("failed call stored rows: %v", rows)
	}

	fake.fail = nil
	result = runDiagnosisShadowPass(context.Background(), database, nil, shadowTestSettings)
	if result.Judged != 1 {
		t.Fatalf("retry pass result = %+v, want the job judged", result)
	}
	if _, ok := shadowRows(t, database)[id]; !ok {
		t.Fatal("job was not judged on the retry pass")
	}
}

func TestDiagnosisShadowPassRateLimitEndsPass(t *testing.T) {
	database := setupShadowTest(t)
	fake := &shadowFake{fail: &typesafe.APIError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", RetryAfter: 30 * time.Second}}
	useShadowFake(t, fake)
	recordShadowFailedJob(t, database, "python a.py", shadowTestLog)
	recordShadowFailedJob(t, database, "python b.py", shadowTestLog)

	result := runDiagnosisShadowPass(context.Background(), database, nil, shadowTestSettings)
	if len(fake.requests) != 1 {
		t.Fatalf("SystemOne called %d times after a 429, want 1", len(fake.requests))
	}
	if !result.RateLimited || result.RetryAfter != 30*time.Second {
		t.Fatalf("pass result = %+v, want rate limited with a 30s retry-after", result)
	}
}

func TestDiagnosisShadowTextWindowsCountCharacters(t *testing.T) {
	s := "héllo wörld"
	if got := headChars(s, 2); got != "hé" {
		t.Errorf("headChars = %q, want hé", got)
	}
	if got := tailChars(s, 4); got != "örld" {
		t.Errorf("tailChars = %q, want örld", got)
	}
	if got := tailChars(s, 100); got != s {
		t.Errorf("tailChars past start = %q, want whole string", got)
	}
}
