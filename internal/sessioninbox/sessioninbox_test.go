package sessioninbox

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestLoadRepresentsAllSessionScopeStatesAndContractFields(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Unix(2_000_000_000, 0)
	exitZero := 0
	exitOne := 1
	record := func(session, project, status string, exitCode *int, endedAt int64) int64 {
		t.Helper()
		jobID, err := db.RecordQueued(database, "cool30", "/tmp/session-inbox", "echo ok", project)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetJobProject(database, jobID, project); err != nil {
			t.Fatal(err)
		}
		if session != "" {
			if err := db.SetJobSubmitterSession(database, jobID, session); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.CloseAttempt(database, jobID, status, exitCode, endedAt); err != nil {
			t.Fatal(err)
		}
		return jobID
	}
	completedID := record("session-one", "augur", db.StatusCompleted, &exitZero, now.Unix()-60)
	if _, err := database.Exec(`UPDATE jobs SET project_root = ? WHERE id = ?`, "/canonical/augur", completedID); err != nil {
		t.Fatal(err)
	}
	failedID := record("session-one", "other", db.StatusFailed, &exitOne, now.Unix()-120)
	record("session-one-child", "augur", db.StatusCompleted, &exitZero, now.Unix()-30)
	record("", "augur", db.StatusCompleted, &exitZero, now.Unix()-10)

	unscoped, err := Load(database, "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if unscoped.Scope.State != ScopeUnscoped || unscoped.Counts.Total != 0 || len(unscoped.Jobs) != 0 {
		t.Fatalf("unscoped query = %+v", unscoped)
	}
	if reminder := FormatReminder(unscoped); reminder != "" {
		t.Fatalf("unscoped reminder = %q, want silence", reminder)
	}

	empty, err := Load(database, "missing-project", "session-one", now)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Scope.State != ScopeScopedEmpty || empty.Counts.Total != 0 || len(empty.Jobs) != 0 {
		t.Fatalf("scoped empty query = %+v", empty)
	}

	populated, err := Load(database, "", "session-one", now)
	if err != nil {
		t.Fatal(err)
	}
	if populated.Version != 1 || populated.Scope.State != ScopeScopedNonEmpty || populated.Scope.SubmitterSession != "session-one" {
		t.Fatalf("version/scope = %+v", populated)
	}
	if populated.Counts != (Counts{Total: 2, Completed: 1, Failed: 1}) {
		t.Fatalf("counts = %+v", populated.Counts)
	}
	if len(populated.Jobs) != 2 || populated.Jobs[0].JobID != "wj"+strconv.FormatInt(completedID, 10) || populated.Jobs[1].JobID != "wj"+strconv.FormatInt(failedID, 10) {
		t.Fatalf("jobs = %+v", populated.Jobs)
	}
	if populated.Jobs[0].Project != "augur" || populated.Jobs[0].ProjectRoot != "/canonical/augur" || populated.Jobs[0].AgeSeconds != 60 || populated.Jobs[0].AgeBasis != "end_time" || populated.Jobs[0].AgeTimestamp != now.Unix()-60 {
		t.Fatalf("completed provenance = %+v", populated.Jobs[0])
	}
	data, err := json.Marshal(populated)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"version":1`, `"state":"scoped_nonempty"`, `"job_id"`, `"project_root":"/canonical/augur"`, `"age_basis":"end_time"`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("contract JSON %s missing %s", data, field)
		}
	}
	if reminder := FormatReminder(populated); !strings.Contains(reminder, "process-results skill") || !strings.Contains(reminder, "2 unprocessed terminal jobs") {
		t.Fatalf("reminder = %q", reminder)
	}
}

func TestLoadGroupsPreservesSessionsNullBucketsAndFourDispositions(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now()
	exitZero, exitOne := 0, 1
	record := func(project, root, session, status string, exitCode *int) int64 {
		t.Helper()
		jobID, err := db.RecordQueued(database, "cool30", "/tmp/"+project, "echo ok", project)
		if err != nil {
			t.Fatal(err)
		}
		var rootValue, sessionValue any
		if root != "" {
			rootValue = root
		}
		if session != "" {
			sessionValue = session
		}
		if _, err := database.Exec(`UPDATE jobs SET project = ?, project_root = ?, submitter_session = ? WHERE id = ?`, project, rootValue, sessionValue, jobID); err != nil {
			t.Fatal(err)
		}
		if err := db.CloseAttempt(database, jobID, status, exitCode, now.Unix()); err != nil {
			t.Fatal(err)
		}
		return jobID
	}

	const root = "/canonical/weft"
	record("weft", root, "session-a", db.StatusCompleted, &exitZero)
	record("weft", root, "session-a", db.StatusCompleted, &exitOne)
	infraID := record("weft", root, "session-a", db.StatusFailed, &exitOne)
	if _, err := database.Exec(`UPDATE job_attempts SET failure_reason = ? WHERE job_id = ?`, db.FailureReasonOOM, infraID); err != nil {
		t.Fatal(err)
	}
	record("weft", root, "session-a", db.StatusDead, nil)
	record("weft", root, "session-b", db.StatusCompleted, &exitZero)
	record("mental-spaces", "", "", db.StatusCompleted, &exitZero)

	query, err := LoadGroups(database)
	if err != nil {
		t.Fatal(err)
	}
	if query.Kind != GroupsKind || query.Version != 1 || query.Scope.State != "all_sessions" {
		t.Fatalf("contract header = %+v", query)
	}
	if len(query.Groups) != 3 {
		t.Fatalf("groups = %+v, want 3", query.Groups)
	}
	find := func(project, session string) Group {
		t.Helper()
		for _, group := range query.Groups {
			if group.Project == project && stringValue(group.SubmitterSession) == session {
				return group
			}
		}
		t.Fatalf("missing group %s/%s in %+v", project, session, query.Groups)
		return Group{}
	}
	a := find("weft", "session-a")
	if a.ProjectRoot == nil || *a.ProjectRoot != root || a.UnattributedSession || a.Total != 4 || a.Dispositions != (Dispositions{CompletedOK: 1, CompletedError: 1, InfraSuspected: 1, Dead: 1}) {
		t.Fatalf("session-a group = %+v", a)
	}
	orphan := find("mental-spaces", "")
	if orphan.ProjectRoot != nil || orphan.SubmitterSession != nil || !orphan.UnattributedSession || orphan.Total != 1 {
		t.Fatalf("explicit null bucket = %+v", orphan)
	}
	data, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"project_root":null`) || !strings.Contains(string(data), `"submitter_session":null`) {
		t.Fatalf("null buckets were omitted from %s", data)
	}
}

func TestAddDispositionFailedAttemptWithoutKnownReasonIsCompletedError(t *testing.T) {
	exitOne := 1
	job := &db.Job{
		Status:           db.StatusFailed,
		RawAttemptStatus: db.StatusFailed,
		ExitCode:         &exitOne,
	}
	var got Dispositions
	addDisposition(&got, job)
	if got != (Dispositions{CompletedError: 1}) {
		t.Fatalf("disposition = %+v, want completed_error", got)
	}
}

func TestAddDispositionFreeFormReasonIsCompletedError(t *testing.T) {
	exitOne := 1
	job := &db.Job{
		Status:        db.StatusFailed,
		ExitCode:      &exitOne,
		FailureReason: "launch completed but job results were not synced from R2",
	}
	var got Dispositions
	addDisposition(&got, job)
	if got != (Dispositions{CompletedError: 1}) {
		t.Fatalf("disposition = %+v, want completed_error", got)
	}
}

func TestAddDispositionKnownReasonPrefixIsCompletedError(t *testing.T) {
	exitOne := 1
	job := &db.Job{
		Status:        db.StatusFailed,
		ExitCode:      &exitOne,
		FailureReason: db.FailureReasonDiskFull + ": cache volume is full",
	}
	var got Dispositions
	addDisposition(&got, job)
	if got != (Dispositions{CompletedError: 1}) {
		t.Fatalf("disposition = %+v, want completed_error", got)
	}
}

func TestAddDispositionKnownForensicReasonsAreInfraSuspected(t *testing.T) {
	reasons := []string{
		db.FailureReasonDiskFull,
		db.FailureReasonOOM,
		db.FailureReasonGPUOOM,
		db.FailureReasonSegfault,
		db.FailureReasonAborted,
		db.FailureReasonKilledSIGKILL,
		db.FailureReasonKilledSIGTERM,
		db.FailureReasonGPUIdle,
		db.FailureReasonStdoutSilence,
		db.FailureReasonSetupTimeout,
		db.FailureReasonRunTimeout,
		db.FailureReasonCUDADriverTooOld,
		db.FailureReasonInfraPrewarmDownloadFailed,
		db.FailureReasonInfraCloudArtifactStageFailed,
		db.FailureReasonInfraTorchPreflightFailed,
		db.FailureReasonInfraCUDAHardwareFault,
	}
	exitOne := 1
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			job := &db.Job{Status: db.StatusFailed, ExitCode: &exitOne, FailureReason: reason}
			var got Dispositions
			addDisposition(&got, job)
			if got != (Dispositions{InfraSuspected: 1}) {
				t.Fatalf("disposition = %+v, want infra_suspected", got)
			}
		})
	}
}

func TestAddDispositionExcludesUserAttributableTorchPreflightReasons(t *testing.T) {
	exitOne := 1
	for _, reason := range []string{
		db.FailureReasonTorchPreflightEnvironmentFailed,
		db.FailureReasonTorchPreflightImportFailed,
	} {
		t.Run(reason, func(t *testing.T) {
			job := &db.Job{Status: db.StatusFailed, ExitCode: &exitOne, FailureReason: reason}
			var got Dispositions
			addDisposition(&got, job)
			if got != (Dispositions{CompletedError: 1}) {
				t.Fatalf("disposition = %+v, want completed_error", got)
			}
		})
	}
}

func TestAddDispositionAssignsExactlyOneBucket(t *testing.T) {
	exitZero, exitOne := 0, 1
	jobs := []*db.Job{
		{Status: db.StatusCompleted, ExitCode: &exitZero},
		{Status: db.StatusCompleted, ExitCode: &exitOne},
		{Status: db.StatusFailed, ExitCode: &exitOne, FailureReason: db.FailureReasonOOM},
		{Status: db.StatusFailed},
		{Status: db.StatusDead},
	}
	for _, job := range jobs {
		var got Dispositions
		addDisposition(&got, job)
		total := got.CompletedOK + got.CompletedError + got.InfraSuspected + got.Dead
		if total != 1 {
			t.Fatalf("job %+v has dispositions %+v, want exactly one bucket", job, got)
		}
	}
}

// TestScopeStateAgreesWithCountedRows pins the contract invariant that
// ScopeScopedNonEmpty and a zero total cannot be reported together. The SQL
// pre-filter and the Go predicate agree today, so the divergent row is
// constructed directly: a killed job is terminal but is not an inbox row, and
// a scope read off the unclassified rows would call that session non-empty.
func TestScopeStateAgreesWithCountedRows(t *testing.T) {
	completed := &db.Job{ID: 1, Status: db.StatusCompleted}
	killed := &db.Job{ID: 2, Status: db.StatusKilled}
	if IsInboxJob(killed) {
		t.Fatal("a killed job must not classify as an inbox row")
	}

	cases := []struct {
		name   string
		scoped bool
		jobs   []*db.Job
		want   ScopeState
	}{
		{"unscoped", false, []*db.Job{completed}, ScopeUnscoped},
		{"no rows", true, nil, ScopeScopedEmpty},
		{"only non-inbox rows", true, []*db.Job{killed}, ScopeScopedEmpty},
		{"one inbox row", true, []*db.Job{completed, killed}, ScopeScopedNonEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, inbox := classify(tc.scoped, "session-one", tc.jobs)
			if scope.State != tc.want {
				t.Fatalf("state = %q, want %q", scope.State, tc.want)
			}
			if tc.scoped && (scope.State == ScopeScopedNonEmpty) != (len(inbox) > 0) {
				t.Fatalf("state %q disagrees with %d kept rows", scope.State, len(inbox))
			}
		})
	}
}

// TestLoadCountsAgreeWithScopeAndJobs guards the same invariant end to end,
// across every scope state the loader can report.
func TestLoadCountsAgreeWithScopeAndJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Unix(2_000_000_000, 0)
	exitZero := 0
	jobID, err := db.RecordQueued(database, "cool30", "/tmp/session-inbox", "echo ok", "augur")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobSubmitterSession(database, jobID, "session-one"); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exitZero, now.Unix()-60); err != nil {
		t.Fatal(err)
	}

	for _, session := range []string{"", "session-one", "session-missing"} {
		query, err := Load(database, "", session, now)
		if err != nil {
			t.Fatal(err)
		}
		if (query.Scope.State == ScopeScopedNonEmpty) != (query.Counts.Total > 0) {
			t.Fatalf("session %q: state %q disagrees with total %d", session, query.Scope.State, query.Counts.Total)
		}
		if query.Counts.Total != len(query.Jobs) {
			t.Fatalf("session %q: total %d disagrees with %d jobs", session, query.Counts.Total, len(query.Jobs))
		}
		if query.Scope.State != ScopeScopedNonEmpty && FormatReminder(query) != "" {
			t.Fatalf("session %q: reminder rendered for state %q", session, query.Scope.State)
		}
	}
}
