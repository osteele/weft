package cmd

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/remediation"
	"github.com/osteele/weft/internal/ssh"
)

func TestFilterJobsByEffectiveStatusExcludesHostlessRunningFromRunning(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "host-a"},
		{ID: 2, Status: db.StatusRunning, Host: ""},
		{ID: 3, Status: db.StatusQueued, Host: ""},
	}

	filtered := jobsWithEffectiveStatus(jobs, db.StatusRunning)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 running job after effective filter, got %d", len(filtered))
	}
	if filtered[0].ID != 1 {
		t.Fatalf("expected job 1 to remain, got job %d", filtered[0].ID)
	}
}

func TestJobsWithEffectiveStatusReclassifiesHostlessRunningAsQueued(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: ""},
		{ID: 2, Status: db.StatusStarting, Host: ""},
		{ID: 3, Status: db.StatusQueued, Host: ""},
		{ID: 4, Status: db.StatusRunning, Host: "host-a"},
	}

	filtered := jobsWithEffectiveStatus(jobs, db.StatusQueued)
	if len(filtered) != 3 {
		t.Fatalf("expected 3 queued jobs after effective reclassification, got %d", len(filtered))
	}
}

func TestPrintJobsDefaultColumnsShowProjectWithoutDirectory(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:          42,
			Host:        "cool30",
			Status:      db.StatusRunning,
			StartTime:   1,
			WorkingDir:  "/workspace/project-alpha",
			Project:     "llm-performance-models",
			Description: "train model",
		},
	}

	output := captureStdout(t, func() {
		if err := printJobs(nil, jobs); err != nil {
			t.Fatalf("printJobs: %v", err)
		}
	})

	if strings.Contains(output, "DIR") {
		t.Fatalf("output unexpectedly includes DIR header, got:\n%s", output)
	}
	if !strings.Contains(output, "PROJECT") {
		t.Fatalf("output missing PROJECT header, got:\n%s", output)
	}
	if !strings.Contains(output, "llm-performance-models") {
		t.Fatalf("output missing project name, got:\n%s", output)
	}
}

func TestFilterJobsByPlacementScope_RentalMatchesTagOrCloudAssignment(t *testing.T) {
	cloudInstanceID := int64(17)
	jobs := []*db.Job{
		{ID: 1, Tags: []string{db.TagRental}},
		{ID: 2, Tags: []string{db.TagCloudLegacy}},
		{ID: 3, Host: db.LaunchHost(cloudInstanceID), LaunchID: &cloudInstanceID},
		{ID: 4, Host: "cool30"},
		{ID: 5, Tags: []string{db.TagInventory}},
	}

	filtered := filterJobsByPlacementScope(jobs, true, false)
	if len(filtered) != 3 {
		t.Fatalf("expected 3 rental jobs, got %d (%+v)", len(filtered), filtered)
	}
}

func TestFilterJobsByPlacementScope_InventoryMatchesInventoryTagOrInventoryHost(t *testing.T) {
	cloudInstanceID := int64(17)
	jobs := []*db.Job{
		{ID: 1, Tags: []string{db.TagInventory}},
		{ID: 2, Host: "cool30"},
		{ID: 3, Host: "", Tags: []string{db.TagInventory}},
		{ID: 4, Tags: []string{db.TagRental}},
		{ID: 5, Host: db.LaunchHost(cloudInstanceID), LaunchID: &cloudInstanceID},
		{ID: 6, Host: ""},
	}

	filtered := filterJobsByPlacementScope(jobs, false, true)
	if len(filtered) != 3 {
		t.Fatalf("expected 3 inventory jobs, got %d (%+v)", len(filtered), filtered)
	}
	for _, job := range filtered {
		if job.UsesRentalPlacement() {
			t.Fatalf("inventory filter should exclude rental jobs, got %+v", job)
		}
	}
}

func TestFilterJobsByPlacementScope_NoFilterReturnsInput(t *testing.T) {
	jobs := []*db.Job{{ID: 1}, {ID: 2}}
	filtered := filterJobsByPlacementScope(jobs, false, false)
	if len(filtered) != len(jobs) {
		t.Fatalf("expected unfiltered jobs, got %d", len(filtered))
	}
}

func TestFilterJobsByHostFlag_StrictHostMatch(t *testing.T) {
	prevHost := listHost
	listHost = "cool30"
	t.Cleanup(func() { listHost = prevHost })

	jobs := []*db.Job{
		{ID: 1, Host: "cool30"},
		{ID: 2, Host: "rental:444"},
		{ID: 3, Host: ""},
		{ID: 4, Host: db.LaunchHost(17)},
	}

	filtered := filterJobsByHostFlag(jobs)
	if len(filtered) != 1 {
		t.Fatalf("expected only 1 host-matching job, got %d", len(filtered))
	}
	if filtered[0].ID != 1 {
		t.Fatalf("expected job 1 to remain, got job %d", filtered[0].ID)
	}
}

func TestFilterJobsByHostFlag_NoHostFlagReturnsInput(t *testing.T) {
	prevHost := listHost
	listHost = ""
	t.Cleanup(func() { listHost = prevHost })

	jobs := []*db.Job{
		{ID: 1, Host: "cool30"},
		{ID: 2, Host: "rental:444"},
		{ID: 3, Host: ""},
	}

	filtered := filterJobsByHostFlag(jobs)
	if len(filtered) != len(jobs) {
		t.Fatalf("expected unfiltered jobs, got %d", len(filtered))
	}
}

func TestFilterJobsSinceUsesLatestLifecycleTimestamp(t *testing.T) {
	cutoff := time.Unix(1_000, 0)
	end := int64(1_100)
	jobs := []*db.Job{
		{ID: 1, CreatedAt: 900, StartTime: 950},
		{ID: 2, CreatedAt: 800, EndTime: &end},
		{ID: 3, CreatedAt: 700, QueuedAt: 1_001},
	}

	filtered := filterJobsSince(jobs, cutoff)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 jobs since cutoff, got %d", len(filtered))
	}
	if filtered[0].ID != 2 || filtered[1].ID != 3 {
		t.Fatalf("unexpected filtered jobs: %+v", filtered)
	}
}

func TestFilterActiveJobsExcludesTerminalStatuses(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued},
		{ID: 2, Status: db.StatusRunning, Host: "cool30"},
		{ID: 3, Status: db.StatusCompleted},
		{ID: 4, Status: db.StatusFailed},
		{ID: 5, Status: db.StatusCanceled},
	}

	filtered := filterActiveJobs(jobs)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 active jobs, got %d", len(filtered))
	}
	if filtered[0].ID != 1 || filtered[1].ID != 2 {
		t.Fatalf("unexpected active jobs: %+v", filtered)
	}
}

func TestListFiltersStatusComposesWithUnprocessed(t *testing.T) {
	prevStatus := listStatus
	prevProcessed := listProcessed
	prevUnprocessed := listUnprocessed
	prevRunning := listRunning
	prevCompleted := listCompleted
	prevQueued := listQueued
	prevDead := listDead
	prevFailed := listFailed
	listStatus = db.StatusFailed
	listProcessed = false
	listUnprocessed = true
	listRunning = false
	listCompleted = false
	listQueued = false
	listDead = false
	listFailed = false
	t.Cleanup(func() {
		listStatus = prevStatus
		listProcessed = prevProcessed
		listUnprocessed = prevUnprocessed
		listRunning = prevRunning
		listCompleted = prevCompleted
		listQueued = prevQueued
		listDead = prevDead
		listFailed = prevFailed
	})

	statusFilter, processedFilter, failedOnly, err := listFilters()
	if err != nil {
		t.Fatalf("listFilters: %v", err)
	}
	if statusFilter != db.StatusFailed || processedFilter != "unprocessed" || failedOnly {
		t.Fatalf("filters = status %q processed %q failedOnly %v, want failed/unprocessed/false",
			statusFilter, processedFilter, failedOnly)
	}
}

func TestWriteWarningsDeduplicatesMessages(t *testing.T) {
	var b strings.Builder
	writeWarnings(&b, []string{"Warning: R2 storage unreachable", "Warning: R2 storage unreachable", "Warning: host slow"})

	out := b.String()
	if strings.Count(out, "Warning: R2 storage unreachable") != 1 {
		t.Fatalf("expected R2 warning once, got:\n%s", out)
	}
	if !strings.Contains(out, "Warning: host slow") {
		t.Fatalf("missing distinct warning, got:\n%s", out)
	}
}

func TestWritePendingRefreshNoticeClearsTerminalLine(t *testing.T) {
	var b strings.Builder
	clearNotice := writePendingRefreshNotice(&b, true)
	clearNotice()

	out := b.String()
	if !strings.Contains(out, "Cloud state may be stale; refreshing in background...") {
		t.Fatalf("missing pending refresh notice, got %q", out)
	}
	if !strings.Contains(out, "\r\x1b[2K") {
		t.Fatalf("missing terminal clear sequence, got %q", out)
	}
	if strings.Contains(out, "\n") {
		t.Fatalf("terminal notice should stay on one erasable line, got %q", out)
	}
}

func TestWritePendingRefreshNoticeNonTerminalDoesNotEmitANSI(t *testing.T) {
	var b strings.Builder
	clearNotice := writePendingRefreshNotice(&b, false)
	clearNotice()

	out := b.String()
	if out != "Cloud state may be stale; refreshing in background...\n" {
		t.Fatalf("notice = %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("non-terminal notice should not include ANSI, got %q", out)
	}
}

func TestValidateListGroupingOptionsUnknownValue(t *testing.T) {
	prevGroupBy := listGroupBy
	prevFormat := listFormat
	listGroupBy = "bogus"
	listFormat = "table"
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
	})

	err := validateListGroupingOptions()
	if err == nil {
		t.Fatal("expected error for unknown group-by value")
	}
	if !strings.Contains(err.Error(), "supported: status, project") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateListGroupingOptionsRejectsJSON(t *testing.T) {
	prevGroupBy := listGroupBy
	prevFormat := listFormat
	listGroupBy = "status"
	listFormat = "json"
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
	})

	err := validateListGroupingOptions()
	if err == nil {
		t.Fatal("expected error for --group-by with json format")
	}
	if !strings.Contains(err.Error(), "table output only") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateListGroupingOptionsRejectsJSONForProject(t *testing.T) {
	prevGroupBy := listGroupBy
	prevFormat := listFormat
	listGroupBy = "project"
	listFormat = "json"
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
	})

	err := validateListGroupingOptions()
	if err == nil {
		t.Fatal("expected error for --group-by with json format")
	}
	if !strings.Contains(err.Error(), "table output only") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveListModeRejectsTUIWithMachineFormat(t *testing.T) {
	prevTUI := listTUI
	prevWatch := listWatch
	prevPlain := listPlain
	prevFormat := listFormat
	prevGroupBy := listGroupBy
	listTUI = true
	listWatch = false
	listPlain = false
	listFormat = "json"
	listGroupBy = ""
	t.Cleanup(func() {
		listTUI = prevTUI
		listWatch = prevWatch
		listPlain = prevPlain
		listFormat = prevFormat
		listGroupBy = prevGroupBy
	})

	_, err := resolveListMode()
	if err == nil {
		t.Fatal("expected --tui with json format to fail")
	}
	if !strings.Contains(err.Error(), "table output only") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveListModePlainWinsForMachineFormat(t *testing.T) {
	prevTUI := listTUI
	prevWatch := listWatch
	prevPlain := listPlain
	prevFormat := listFormat
	prevGroupBy := listGroupBy
	listTUI = false
	listWatch = false
	listPlain = false
	listFormat = "tsv"
	listGroupBy = ""
	t.Cleanup(func() {
		listTUI = prevTUI
		listWatch = prevWatch
		listPlain = prevPlain
		listFormat = prevFormat
		listGroupBy = prevGroupBy
	})

	useTUI, err := resolveListMode()
	if err != nil {
		t.Fatalf("resolveListMode: %v", err)
	}
	if useTUI {
		t.Fatal("machine format should use plain mode")
	}
}

func TestRunListPlainDefaultDoesNotSync(t *testing.T) {
	stubEmptyQueueStatus(t)

	database := db.SetupTestDB(t)
	if _, err := db.RecordQueuedWithGPU(database, "studio", "/tmp", "echo hi", "cached list", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	restoreListFlags(t)
	listAll = true
	listLimit = 10

	originalSync := syncListDataFunc
	t.Cleanup(func() {
		syncListDataFunc = originalSync
	})
	calls := 0
	syncListDataFunc = func(_ *sql.DB) []string {
		calls++
		return nil
	}

	captureStdout(t, func() {
		if err := runListPlain(database, nil); err != nil {
			t.Fatalf("runListPlain: %v", err)
		}
	})

	if calls != 0 {
		t.Fatalf("default plain list sync calls = %d, want 0", calls)
	}
}

func TestRunListPlainExplicitSyncRefreshes(t *testing.T) {
	stubEmptyQueueStatus(t)

	database := db.SetupTestDB(t)
	if _, err := db.RecordQueuedWithGPU(database, "studio", "/tmp", "echo hi", "synced list", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	restoreListFlags(t)
	listAll = true
	listLimit = 10
	listSync = true

	originalSync := syncListDataFunc
	t.Cleanup(func() {
		syncListDataFunc = originalSync
	})
	calls := 0
	syncListDataFunc = func(_ *sql.DB) []string {
		calls++
		return nil
	}

	captureStdout(t, func() {
		if err := runListPlain(database, nil); err != nil {
			t.Fatalf("runListPlain: %v", err)
		}
	})

	if calls != 1 {
		t.Fatalf("explicit plain list sync calls = %d, want 1", calls)
	}
}

func TestCollectJobsForListWithFiltersOverridesProject(t *testing.T) {
	database := db.SetupTestDB(t)
	alphaID, err := db.RecordQueued(database, "studio", "/tmp/alpha", "echo alpha", "")
	if err != nil {
		t.Fatalf("record alpha: %v", err)
	}
	betaID, err := db.RecordQueued(database, "studio", "/tmp/beta", "echo beta", "")
	if err != nil {
		t.Fatalf("record beta: %v", err)
	}
	if err := db.SetJobProject(database, alphaID, "alpha"); err != nil {
		t.Fatalf("project alpha: %v", err)
	}
	if err := db.SetJobProject(database, betaID, "beta"); err != nil {
		t.Fatalf("project beta: %v", err)
	}

	prevProject := listProject
	prevLimit := listLimit
	prevAll := listAll
	listProject = ""
	listLimit = 0
	listAll = true
	t.Cleanup(func() {
		listProject = prevProject
		listLimit = prevLimit
		listAll = prevAll
	})

	jobs, err := collectJobsForListWithFilters(database, nil, "", "", "alpha")
	if err != nil {
		t.Fatalf("collectJobsForListWithFilters: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != alphaID {
		t.Fatalf("jobs = %+v, want alpha job %d", jobs, alphaID)
	}
	if listProject != "" {
		t.Fatalf("listProject leaked as %q", listProject)
	}
}

func TestPrintJobsGroupedStatus(t *testing.T) {
	stubEmptyQueueStatus(t)

	prevGroupBy := listGroupBy
	prevFormat := listFormat
	listGroupBy = "status"
	listFormat = "table"
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
	})

	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "cool30", Project: "proj", Description: "run"},
		{ID: 2, Status: db.StatusQueued, Host: "cool30", Project: "proj", Description: "wait"},
	}
	out := captureStdout(t, func() {
		if err := printJobs(nil, jobs); err != nil {
			t.Fatalf("printJobs: %v", err)
		}
	})

	if !strings.Contains(out, "Running (1):") {
		t.Fatalf("expected grouped running section, got:\n%s", out)
	}
	if !strings.Contains(out, "Queued (1):") {
		t.Fatalf("expected grouped queued section, got:\n%s", out)
	}
}

func TestPrintJobsGroupedProject(t *testing.T) {
	stubEmptyQueueStatus(t)

	prevGroupBy := listGroupBy
	prevFormat := listFormat
	listGroupBy = "project"
	listFormat = "table"
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
	})

	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "cool30", Project: "proj-a", WorkingDir: "/tmp/proj-a", Description: "run"},
		{ID: 2, Status: db.StatusQueued, Host: "cool100", Project: "proj-b", WorkingDir: "/tmp/proj-b", Description: "wait"},
	}
	out := captureStdout(t, func() {
		if err := printJobs(nil, jobs); err != nil {
			t.Fatalf("printJobs: %v", err)
		}
	})

	for _, want := range []string{"proj-a", "proj-b", "dir: /tmp/proj-a", "dir: /tmp/proj-b", "DESCRIPTION"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestPrintJobsGroupedStatusUnprocessedExcludesCanceledKeepsKilled(t *testing.T) {
	prevGroupBy := listGroupBy
	prevFormat := listFormat
	prevUnprocessed := listUnprocessed
	prevProcessed := listProcessed
	prevStatus := listStatus
	listGroupBy = "status"
	listFormat = "table"
	listUnprocessed = true
	listProcessed = false
	listStatus = ""
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
		listUnprocessed = prevUnprocessed
		listProcessed = prevProcessed
		listStatus = prevStatus
	})

	jobs := []*db.Job{
		{ID: 8, Status: db.StatusKilled, Host: "cool30", Project: "proj", Description: "killed"},
		{ID: 9, Status: db.StatusCanceled, Host: "cool30", Project: "proj", Description: "canceled"},
	}

	out := captureStdout(t, func() {
		if err := printJobs(nil, jobs); err != nil {
			t.Fatalf("printJobs: %v", err)
		}
	})

	if !strings.Contains(out, "Killed/Canceled (1):") {
		t.Fatalf("expected grouped killed/canceled section with one job, got:\n%s", out)
	}
	if !strings.Contains(out, "killed") {
		t.Fatalf("expected killed job line, got:\n%s", out)
	}
	if strings.Contains(out, "canceled") {
		t.Fatalf("did not expect canceled job line in grouped unprocessed output, got:\n%s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(data)
}

func stubEmptyQueueStatus(t *testing.T) {
	t.Helper()
	cleanupSSH := ssh.SetRunner(func(string, string) (string, string, error) {
		return "", "", nil
	})
	t.Cleanup(cleanupSSH)
}

func restoreListFlags(t *testing.T) {
	t.Helper()

	origRunning := listRunning
	origCompleted := listCompleted
	origQueued := listQueued
	origDead := listDead
	origFailed := listFailed
	origStatus := listStatus
	origHost := listHost
	origSince := listSince
	origMine := listMine
	origActive := listActive
	origAllHosts := listAllHosts
	origSearch := listSearch
	origLimit := listLimit
	origSync := listSync
	origNoSync := listNoSync
	origAll := listAll
	origTags := listTags
	origExcludeTags := listExcludeTags
	origProject := listProject
	origProcessed := listProcessed
	origUnprocessed := listUnprocessed
	origRental := listRental
	origInventory := listInventory
	origCloud := listCloud
	origFormat := listFormat
	origNoTruncate := listNoTruncate
	origColumns := listColumns
	origGroupBy := listGroupBy

	t.Cleanup(func() {
		listRunning = origRunning
		listCompleted = origCompleted
		listQueued = origQueued
		listDead = origDead
		listFailed = origFailed
		listStatus = origStatus
		listHost = origHost
		listSince = origSince
		listMine = origMine
		listActive = origActive
		listAllHosts = origAllHosts
		listSearch = origSearch
		listLimit = origLimit
		listSync = origSync
		listNoSync = origNoSync
		listAll = origAll
		listTags = origTags
		listExcludeTags = origExcludeTags
		listProject = origProject
		listProcessed = origProcessed
		listUnprocessed = origUnprocessed
		listRental = origRental
		listInventory = origInventory
		listCloud = origCloud
		listFormat = origFormat
		listNoTruncate = origNoTruncate
		listColumns = origColumns
		listGroupBy = origGroupBy
	})

	listRunning = false
	listCompleted = false
	listQueued = false
	listDead = false
	listFailed = false
	listStatus = ""
	listHost = ""
	listSince = ""
	listMine = false
	listActive = false
	listAllHosts = false
	listSearch = ""
	listLimit = 50
	listSync = false
	listNoSync = false
	listAll = false
	listTags = nil
	listExcludeTags = nil
	listProject = ""
	listProcessed = false
	listUnprocessed = false
	listRental = false
	listInventory = false
	listCloud = false
	listFormat = "table"
	listNoTruncate = false
	listColumns = nil
	listGroupBy = ""
}

// recordFailedJob opens a fresh temp DB for the test, records a job, and
// marks its latest attempt failed with exit 1. Returns the DB handle and
// the new job ID. The DB and temp file are cleaned up via t.Cleanup.
func recordFailedJob(t *testing.T, namePrefix string) (*sql.DB, int64) {
	t.Helper()
	tmpfile, err := os.CreateTemp("", namePrefix+"-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	tmpfile.Close()
	t.Cleanup(func() { os.Remove(tmpfile.Name()) })

	t.Cleanup(db.SetDBPath(tmpfile.Name()))

	database, err := db.Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	jobID, err := db.RecordJobStarting(database, "cool100", "/tmp/proj", "uv run job.py", "test job")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	exit := 1
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, exit_code = ?, end_time = ? WHERE job_id = ?`,
		db.StatusFailed, exit, time.Now().Unix(), jobID,
	); err != nil {
		t.Fatalf("mark attempt failed: %v", err)
	}
	return database, jobID
}

func TestShowJob_SurfacesStoredDiagnosis(t *testing.T) {
	database, jobID := recordFailedJob(t, "showjob-stored")

	diagJSON := mustMarshalDiagnosis(t, &remediation.ErrorDiagnosis{
		Pattern:           "gpu_oom",
		Message:           "GPU out of memory",
		Solution:          "Reduce batch size or request more GPU memory",
		GPUOOMMainPID:     12345,
		GPUOOMProcesses:   []remediation.GPUOOMProcess{{PID: 12345, MemoryGiB: 78.42}},
		GPUOOMHintDeltaGB: 8,
	})
	if err := db.SetAttemptErrorDiagnosis(database, jobID, diagJSON); err != nil {
		t.Fatalf("set diagnosis: %v", err)
	}

	out := captureStdout(t, func() {
		if err := showJob(database, jobID); err != nil {
			t.Fatalf("showJob: %v", err)
		}
	})

	for _, want := range []string{
		"Diagnosis: GPU out of memory (gpu_oom)",
		"Solution:  Reduce batch size or request more GPU memory",
		"           main GPU process pid=12345 using 78.42 GiB",
		"           hint: increase --gpu-mem by ~8GB on retry",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("showJob output missing %q in:\n%s", want, out)
		}
	}
}

func TestShowJob_ShowsDeclaredInputs(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/proj", "uv run eval.py", "input visibility", "nvidia")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	inputs := []string{"hf:Qwen/Qwen2.5-0.5B", "hf:EleutherAI/pythia-160m"}
	if err := db.SetJobInputs(database, jobID, inputs); err != nil {
		t.Fatalf("SetJobInputs: %v", err)
	}
	if err := db.SetJobMetadata(database, jobID, &db.JobMetadata{
		BestEffortInputs: []string{"hf:EleutherAI/pythia-160m"},
	}); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}

	out := captureStdout(t, func() {
		if err := showJob(database, jobID); err != nil {
			t.Fatalf("showJob: %v", err)
		}
	})

	for _, want := range []string{
		"Inputs:       hf:Qwen/Qwen2.5-0.5B, hf:EleutherAI/pythia-160m",
		"Best Effort:  hf:EleutherAI/pythia-160m",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("showJob output missing %q in:\n%s", want, out)
		}
	}
}

// TestShowJob_LogCacheFallback covers the EXP-179 regression: a job whose
// remediation pipeline never ran (benchmark / processed / no-retry) leaves
// error_diagnosis empty, so the diagnostic must come from a live scan of
// the local log cache.
func TestShowJob_LogCacheFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	database, jobID := recordFailedJob(t, "showjob-fallback")

	logPath := logcache.CachePath(jobID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := logcache.Write(jobID, "torch.cuda.OutOfMemoryError: CUDA out of memory. Tried to allocate 4.70 GiB.\n"); err != nil {
		t.Fatalf("write log cache: %v", err)
	}

	out := captureStdout(t, func() {
		if err := showJob(database, jobID); err != nil {
			t.Fatalf("showJob: %v", err)
		}
	})

	if !strings.Contains(out, "(gpu_oom)") {
		t.Errorf("showJob did not surface gpu_oom pattern via log-cache fallback, got:\n%s", out)
	}
}
