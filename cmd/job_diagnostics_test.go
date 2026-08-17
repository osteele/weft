package cmd

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/remediation"
)

func mustMarshalDiagnosis(t *testing.T, d *remediation.ErrorDiagnosis) string {
	t.Helper()
	s, err := remediation.MarshalDiagnosis(d)
	if err != nil {
		t.Fatalf("marshal diagnosis: %v", err)
	}
	return s
}

// TestResolveJobDiagnosis covers the stored→log-cache fallback matrix.
// The fallback is what lets `weft job info` / `weft job status` surface
// a diagnosis for jobs that never invoked the remediation pipeline (the
// EXP-179 wj2041 regression — see specs/job-lifecycle.allium).
func TestResolveJobDiagnosis(t *testing.T) {
	storedOOM := mustMarshalDiagnosis(t, &remediation.ErrorDiagnosis{
		Pattern: "module_not_found", Category: "environment", Message: "missing module: torch",
	})
	zero := 0
	two := 2
	sigterm := 143

	cases := []struct {
		name          string
		job           *db.Job
		logContent    string
		writeEmptyLog bool
		wantPattern   string // "" = expect nil result
	}{
		{
			name: "nil job returns nil",
			job:  nil,
		},
		{
			name:        "stored wins over log content",
			job:         &db.Job{ID: 42, ErrorDiagnosis: storedOOM},
			logContent:  "torch.cuda.OutOfMemoryError: CUDA out of memory\n",
			wantPattern: "module_not_found",
		},
		{
			name:        "fallback to log when stored is empty",
			job:         &db.Job{ID: 2041},
			logContent:  "RuntimeError: CUDA out of memory. Tried to allocate 4.70 GiB.\n",
			wantPattern: "gpu_oom",
		},
		{
			name:        "corrupt stored falls through to log",
			job:         &db.Job{ID: 7, ErrorDiagnosis: "{not valid json"},
			logContent:  "torch.cuda.OutOfMemoryError\n",
			wantPattern: "gpu_oom",
		},
		{
			name:       "clean completed job ignores stale log-cache diagnosis",
			job:        &db.Job{ID: 4615, Status: db.StatusCompleted, ExitCode: &zero},
			logContent: "Execution timed out after 4h\nRuntimeError: CUDA out of memory\n",
		},
		{
			name: "no stored and no log returns nil",
			job:  &db.Job{ID: 9999},
		},
		{
			name:          "empty log file returns nil",
			job:           &db.Job{ID: 5},
			writeEmptyLog: true,
		},
		{
			// wb68: wj6237 exited 2 because a pre-registered gate reported
			// its verdict, 2m39s into a 2h cap. The only timeout token in
			// the log was weft's own preflight line, and it made a result
			// read as an infrastructure failure worth re-running.
			name:       "deliberate non-zero exit is not a timeout",
			job:        &db.Job{ID: 6237, Status: db.StatusFailed, ExitCode: &two},
			logContent: "weft: torch preflight timed out after 30s; continuing\n[RESULT] status=failed-prerequisite\n=== END exit=2 ===\n",
		},
		{
			// The same guard must not swallow a real deadline kill, whose
			// signature is death by signal rather than a chosen exit code.
			name:        "SIGTERM exit keeps its timeout diagnosis",
			job:         &db.Job{ID: 6238, Status: db.StatusFailed, ExitCode: &sigterm},
			logContent:  "Execution exceeded maximum time budget\n=== END exit=143 ===\n",
			wantPattern: "timeout",
		},
		{
			// The suppression applies to a stored diagnosis too, not just
			// the log-cache fallback.
			name: "stored timeout dropped for a chosen exit code",
			job: &db.Job{ID: 6239, Status: db.StatusFailed, ExitCode: &two, ErrorDiagnosis: mustMarshalDiagnosis(t, &remediation.ErrorDiagnosis{
				Pattern: "timeout", Category: "environment", Message: "Execution timed out",
			})},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempHome := t.TempDir()
			t.Setenv("HOME", tempHome)
			switch {
			case tc.writeEmptyLog:
				logPath := logcache.CachePath(tc.job.ID)
				if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
					t.Fatalf("mkdir cache: %v", err)
				}
				if err := os.WriteFile(logPath, nil, 0o644); err != nil {
					t.Fatalf("write empty log: %v", err)
				}
			case tc.logContent != "":
				if err := logcache.Write(tc.job.ID, tc.logContent); err != nil {
					t.Fatalf("write log cache: %v", err)
				}
			}

			got := ResolveJobDiagnosis(tc.job)
			switch {
			case tc.wantPattern == "" && got != nil:
				t.Errorf("got %+v, want nil", got)
			case tc.wantPattern != "" && got == nil:
				t.Errorf("got nil, want pattern %q", tc.wantPattern)
			case tc.wantPattern != "" && got.Pattern != tc.wantPattern:
				t.Errorf("Pattern = %q, want %q", got.Pattern, tc.wantPattern)
			}
		})
	}
}

// TestHasFailureSignal gates the local-diagnostics block: in-progress and
// clean-completed jobs must skip the log-cache scan to keep the hot path
// cheap.
func TestHasFailureSignal(t *testing.T) {
	nonZero := 1
	zero := 0
	cases := []struct {
		name string
		job  *db.Job
		want bool
	}{
		{"nil", nil, false},
		{"queued", &db.Job{Status: db.StatusQueued}, false},
		{"running", &db.Job{Status: db.StatusRunning}, false},
		{"clean completed", &db.Job{Status: db.StatusCompleted, ExitCode: &zero}, false},
		{"failed status", &db.Job{Status: db.StatusFailed}, true},
		{"dead status", &db.Job{Status: db.StatusDead}, true},
		{"killed status", &db.Job{Status: db.StatusKilled}, true},
		{"non-zero exit", &db.Job{Status: db.StatusCompleted, ExitCode: &nonZero}, true},
		{"failure metadata on completed status", &db.Job{Status: db.StatusCompleted, FailureReason: "gpu_oom"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasFailureSignal(tc.job); got != tc.want {
				t.Errorf("hasFailureSignal = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestJobPublicationDiagnosticLinesPreserveUnknownQuantities(t *testing.T) {
	database := db.SetupTestDB(t)
	const jobID = int64(4616)
	if _, err := database.Exec(`INSERT INTO jobs (id, command, working_dir, created_at) VALUES (?, 'true', '/tmp', 1)`, jobID); err != nil {
		t.Fatal(err)
	}
	attemptID, err := db.CreateAttempt(database, jobID, "host-alpha", nil, db.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	progress := int64(110)
	if _, err := db.UpsertAttemptPublicationState(database, &db.AttemptPublicationState{
		AttemptID: attemptID, JobID: jobID, Sequence: 1, ObservedAt: 120,
		ExecutionState:         db.PublicationExecutionComplete,
		RequiredArtifactsState: db.PublicationStateUnknown,
		DrainState:             db.PublicationStatePending,
		QueuedItems:            &zero, QueuedBytes: nil, LastProgressAt: &progress,
		UnknownReason: "publication report lookup timed out",
	}); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Join(jobPublicationDiagnosticLines(database, job), "\n")
	for _, want := range []string{"Execution: complete", "Artifacts: unknown", "Drain: pending", "0 item(s) remaining", "Publication unknown: publication report lookup timed out"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("diagnostics missing %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "0 B remaining") {
		t.Fatalf("unknown bytes rendered as zero:\n%s", lines)
	}
}

// Regression (wb18): weft info must show the constraint that actually
// rejects hosts (the driver/CUDA floor) with its provenance, not just the
// usually-inert arch cap.
func TestDriverFloorLine_TorchPinProvenance(t *testing.T) {
	dir := t.TempDir()
	dataloc.WriteTestTorchPin(t, dir, "2.9.1", "cu128")
	mem := 8
	job := &db.Job{WorkingDir: dir, Command: "uv run train.py", GPUMemGB: &mem}

	line := driverFloorLine(job)
	if !strings.Contains(line, ">=570") {
		t.Errorf("driverFloorLine = %q, want driver >=570 (operational floor)", line)
	}
	if !strings.Contains(line, "CUDA >=12.8") || !strings.Contains(line, "torch pin") {
		t.Errorf("driverFloorLine = %q, want cloud CUDA floor with torch provenance", line)
	}
}

func TestDriverFloorLine_UsesCloudExactTorchCUDAFloor(t *testing.T) {
	dir := t.TempDir()
	dataloc.WriteTestTorchPin(t, dir, "2.6.0", "cu124")
	mem := 20
	job := &db.Job{WorkingDir: dir, Command: "uv run train.py", GPUMemGB: &mem}

	line := driverFloorLine(job)
	if !strings.Contains(line, ">=550") {
		t.Errorf("driverFloorLine = %q, want cloud driver >=550", line)
	}
	if !strings.Contains(line, "CUDA >=12.4") {
		t.Errorf("driverFloorLine = %q, want exact cloud CUDA floor 12.4", line)
	}
}

func TestDriverFloorLine_NoTorchPin(t *testing.T) {
	mem := 8
	job := &db.Job{WorkingDir: t.TempDir(), Command: "python x.py", GPUMemGB: &mem}
	if line := driverFloorLine(job); line != "" {
		t.Errorf("driverFloorLine = %q, want empty for project without torch pin", line)
	}
}

// printJobStatus is wired straight into terminal.Dependencies.PrintJobStatus;
// this pins the signature that carries the database through to plain watch
// (wb73), so a future change cannot silently drop the handle again.
var _ func(*sql.DB, *db.Job, bool) = printJobStatus

// TestJobDiagnosticsToleratesNilDatabase guards wb73: these helpers must
// degrade to "no diagnostics" on a nil database rather than panic in
// db.GetLaunch and take the watch process down.
func TestJobDiagnosticsToleratesNilDatabase(t *testing.T) {
	launchID := int64(7290)
	job := &db.Job{ID: 6269, Status: db.StatusFailed, LaunchID: &launchID}

	printLaunchTerminationDetail(nil, job)
	printJobPhasesAndPeaks(nil, job)
	printJobLocalDiagnostics(nil, job)
	printLaunchTerminationDetail(nil, nil)
	printJobPhasesAndPeaks(nil, nil)
}

// With a real handle the same helpers must reach their lookups and tolerate a
// miss — a job whose launch row is absent yields no detail, not an error path.
func TestJobDiagnosticsWithDatabaseHandlesMissingLaunch(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID := int64(7290)
	job := &db.Job{ID: 6269, Status: db.StatusFailed, LaunchID: &launchID}

	printLaunchTerminationDetail(database, job)
	printJobPhasesAndPeaks(database, job)
	printJobLocalDiagnostics(database, job)
}
