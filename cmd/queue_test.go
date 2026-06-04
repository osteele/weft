package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
)

func TestBuildQueueEditDependencies(t *testing.T) {
	database := db.SetupTestDB(t)

	targetID, err := db.RecordQueued(database, "hostA", "/tmp", "echo target", "target")
	if err != nil {
		t.Fatalf("record target job: %v", err)
	}
	successID, err := db.RecordQueued(database, "hostA", "/tmp", "echo success", "success")
	if err != nil {
		t.Fatalf("record success job: %v", err)
	}
	anyID, err := db.RecordQueued(database, "hostA", "/tmp", "echo any", "any")
	if err != nil {
		t.Fatalf("record completion job: %v", err)
	}

	deps, cloudAfter, err := buildQueueEditDependencies(database, "hostA", targetID,
		[]string{fmt.Sprintf("%d", successID)},
		[]string{fmt.Sprintf("%d", anyID)},
	)
	if err != nil {
		t.Fatalf("build dependencies: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d", len(deps))
	}
	if deps[0].JobID != successID || deps[0].AllowFailure {
		t.Fatalf("expected success dep to require success, got %+v", deps[0])
	}
	if deps[1].JobID != anyID || !deps[1].AllowFailure {
		t.Fatalf("expected completion dep to allow failure, got %+v", deps[1])
	}
	if len(cloudAfter) != 0 {
		t.Fatalf("expected no cloud deps, got %+v", cloudAfter)
	}

	// Self-dependency should fail
	if _, _, err := buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d", targetID)}, nil); err == nil {
		t.Fatal("expected self-dependency to error")
	}

	// Cross-host dependency should fail
	otherHostID, err := db.RecordQueued(database, "hostB", "/tmp", "echo other", "other")
	if err != nil {
		t.Fatalf("record other host job: %v", err)
	}
	_, _, err = buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d", otherHostID)}, nil)
	if err == nil || !errors.Is(err, errCrossHostDep) {
		t.Fatalf("expected errCrossHostDep, got %v", err)
	}

	// Hostless/rental dependency should become cloud dependency metadata
	hostlessID, err := db.RecordQueued(database, "", "/tmp", "echo cloud", "cloud")
	if err != nil {
		t.Fatalf("record hostless job: %v", err)
	}
	deps, cloudAfter, err = buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d", hostlessID)}, nil)
	if err != nil {
		t.Fatalf("hostless dependency: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("expected no local deps for hostless job, got %+v", deps)
	}
	if len(cloudAfter) != 1 || cloudAfter[0].JobID != hostlessID {
		t.Fatalf("expected cloud dep for hostless job, got %+v", cloudAfter)
	}
}

func TestBuildQueueEditDependenciesModes(t *testing.T) {
	database := db.SetupTestDB(t)
	targetID, err := db.RecordQueued(database, "hostA", "/tmp", "echo target", "target")
	if err != nil {
		t.Fatalf("record target job: %v", err)
	}
	jobA, err := db.RecordQueued(database, "hostA", "/tmp", "echo A", "A")
	if err != nil {
		t.Fatalf("record job A: %v", err)
	}
	jobB, err := db.RecordQueued(database, "hostA", "/tmp", "echo B", "B")
	if err != nil {
		t.Fatalf("record job B: %v", err)
	}

	deps, _, err := buildQueueEditDependencies(database, "hostA", targetID,
		[]string{fmt.Sprintf("%d:any,%d:success", jobA, jobB)},
		nil,
	)
	if err != nil {
		t.Fatalf("build dependencies: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d", len(deps))
	}
	if deps[0].JobID != jobA || !deps[0].AllowFailure {
		t.Fatalf("expected job A to allow failure, got %+v", deps[0])
	}
	if deps[1].JobID != jobB || deps[1].AllowFailure {
		t.Fatalf("expected job B to require success, got %+v", deps[1])
	}
}

func TestBuildQueueEditDependenciesValidation(t *testing.T) {
	database := db.SetupTestDB(t)
	targetID, err := db.RecordQueued(database, "hostA", "/tmp", "echo target", "target")
	if err != nil {
		t.Fatalf("record target job: %v", err)
	}
	jobA, err := db.RecordQueued(database, "hostA", "/tmp", "echo A", "A")
	if err != nil {
		t.Fatalf("record job A: %v", err)
	}

	// '+' suffix on first entry should set allow-failure
	deps, _, err := buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d+", jobA)}, nil)
	if err != nil {
		t.Fatalf("plus suffix: %v", err)
	}
	if len(deps) != 1 || !deps[0].AllowFailure {
		t.Fatalf("expected allow failure from suffix, got %+v", deps)
	}

	// Duplicated entries keep the first interpretation
	deps, _, err = buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d,%d+", jobA, jobA)}, nil)
	if err != nil {
		t.Fatalf("dedupe deps: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep after dedupe, got %d", len(deps))
	}
	if deps[0].AllowFailure {
		t.Fatalf("expected first entry to win when deduping")
	}

	// Unknown mode
	_, _, err = buildQueueEditDependencies(database, "hostA", targetID, []string{fmt.Sprintf("%d:bogus", jobA)}, nil)
	if err == nil || !errors.Is(err, errUnknownDepMode) {
		t.Fatalf("expected errUnknownDepMode, got %v", err)
	}

	// Non-numeric ID
	_, _, err = buildQueueEditDependencies(database, "hostA", targetID, []string{"abc"}, nil)
	if err == nil || !errors.Is(err, errInvalidDepJobID) {
		t.Fatalf("expected errInvalidDepJobID, got %v", err)
	}
}

func TestSplitAndFormatDependencies(t *testing.T) {
	values := splitDependencyValues("1, 2 ,\t3")
	if len(values) != 3 || values[1] != "2" {
		t.Fatalf("unexpected split result: %#v", values)
	}
	if result := splitDependencyValues("   "); result != nil {
		t.Fatalf("expected nil result for empty string, got %#v", result)
	}

	deps := []queueDependency{
		{JobID: 10, AllowFailure: false},
		{JobID: 11, AllowFailure: true},
	}
	formatted := formatQueueDependencies(deps)
	if !strings.Contains(formatted, "10 (success)") || !strings.Contains(formatted, "11 (completion)") {
		t.Fatalf("unexpected format: %s", formatted)
	}
	if formatQueueDependencies(nil) != "" {
		t.Fatalf("expected empty string for empty deps")
	}
}

func TestDecodeQueueDependencies(t *testing.T) {
	spec := "10,11:any,foo"
	deps := decodeQueueDependencies(spec)
	if len(deps) != 2 {
		t.Fatalf("expected 2 deps, got %d", len(deps))
	}
	if deps[0].JobID != 10 || deps[0].AllowFailure {
		t.Fatalf("unexpected first dep: %+v", deps[0])
	}
	if deps[1].JobID != 11 || !deps[1].AllowFailure {
		t.Fatalf("unexpected second dep: %+v", deps[1])
	}
	if out := formatQueueDependencies(decodeQueueDependencies("")); out != "" {
		t.Fatalf("expected empty decode, got %s", out)
	}
}

func TestRunEditReplacesTags(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobTags(database, jobID, []string{"old", "train"}); err != nil {
		t.Fatalf("set initial tags: %v", err)
	}

	resetEditState()
	cmd := newEditTestCommand()
	if err := cmd.Flags().Set("tag", "benchmark,processed,benchmark, "); err != nil {
		t.Fatalf("set tag flag: %v", err)
	}

	var remoteTags []string
	cleanupSSH := ssh.SetRunner(func(host, command string) (string, string, error) {
		if host != "hostA" {
			return "", "", fmt.Errorf("unexpected host %q", host)
		}
		if strings.Contains(command, fmt.Sprintf("job-%d.json", jobID)) {
			remoteTags, err = extractQueuedJobTags(command, jobID)
			if err != nil {
				t.Fatalf("extract queued tags: %v", err)
			}
			return "", "", nil
		}
		return "", "connection timed out", fmt.Errorf("exit status 255")
	})
	t.Cleanup(cleanupSSH)

	out := captureStdout(t, func() {
		if err := runEdit(cmd, []string{fmt.Sprintf("%d", jobID)}); err != nil {
			t.Fatalf("runEdit: %v", err)
		}
	})
	if !strings.Contains(out, "tags: benchmark, processed") {
		t.Fatalf("output missing tag update, got %q", out)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	wantTags := []string{"benchmark", "processed"}
	if got := job.Tags; len(got) != len(wantTags) {
		t.Fatalf("tags len = %d, want %d (%v)", len(got), len(wantTags), got)
	} else {
		for i := range wantTags {
			if got[i] != wantTags[i] {
				t.Fatalf("tags[%d] = %q, want %q", i, got[i], wantTags[i])
			}
		}
	}

	if len(remoteTags) != len(wantTags) {
		t.Fatalf("remote tags len = %d, want %d (%v)", len(remoteTags), len(wantTags), remoteTags)
	}
	for i := range wantTags {
		if remoteTags[i] != wantTags[i] {
			t.Fatalf("remoteTags[%d] = %q, want %q", i, remoteTags[i], wantTags[i])
		}
	}
}

func TestRunEditClearsTags(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobTags(database, jobID, []string{"old", "train"}); err != nil {
		t.Fatalf("set initial tags: %v", err)
	}

	resetEditState()
	cmd := newEditTestCommand()
	if err := cmd.Flags().Set("clear-tags", "true"); err != nil {
		t.Fatalf("set clear-tags flag: %v", err)
	}

	var remoteTags []string
	cleanupSSH := ssh.SetRunner(func(host, command string) (string, string, error) {
		if host != "hostA" {
			return "", "", fmt.Errorf("unexpected host %q", host)
		}
		if strings.Contains(command, fmt.Sprintf("job-%d.json", jobID)) {
			remoteTags, err = extractQueuedJobTags(command, jobID)
			if err != nil {
				t.Fatalf("extract queued tags: %v", err)
			}
			return "", "", nil
		}
		return "", "connection timed out", fmt.Errorf("exit status 255")
	})
	t.Cleanup(cleanupSSH)

	out := captureStdout(t, func() {
		if err := runEdit(cmd, []string{fmt.Sprintf("%d", jobID)}); err != nil {
			t.Fatalf("runEdit: %v", err)
		}
	})
	if !strings.Contains(out, "tags cleared") {
		t.Fatalf("output missing clear message, got %q", out)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if len(job.Tags) != 0 {
		t.Fatalf("expected tags cleared, got %v", job.Tags)
	}
	if len(remoteTags) != 0 {
		t.Fatalf("expected remote tags cleared, got %v", remoteTags)
	}
}

func TestRunEditRemovesTag(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobTags(database, jobID, []string{"benchmark", "exp-012"}); err != nil {
		t.Fatalf("set initial tags: %v", err)
	}

	resetEditState()
	cmd := newEditTestCommand()
	if err := cmd.Flags().Set("remove-tag", "benchmark"); err != nil {
		t.Fatalf("set remove-tag flag: %v", err)
	}

	var remoteTags []string
	cleanupSSH := ssh.SetRunner(func(host, command string) (string, string, error) {
		if host != "hostA" {
			return "", "", fmt.Errorf("unexpected host %q", host)
		}
		if strings.Contains(command, fmt.Sprintf("job-%d.json", jobID)) {
			remoteTags, err = extractQueuedJobTags(command, jobID)
			if err != nil {
				t.Fatalf("extract queued tags: %v", err)
			}
			return "", "", nil
		}
		return "", "connection timed out", fmt.Errorf("exit status 255")
	})
	t.Cleanup(cleanupSSH)

	out := captureStdout(t, func() {
		if err := runEdit(cmd, []string{fmt.Sprintf("%d", jobID)}); err != nil {
			t.Fatalf("runEdit: %v", err)
		}
	})
	if !strings.Contains(out, "tags: exp-012") {
		t.Fatalf("output missing tag update, got %q", out)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	wantTags := []string{"exp-012"}
	if got := job.Tags; len(got) != len(wantTags) {
		t.Fatalf("tags len = %d, want %d (%v)", len(got), len(wantTags), got)
	} else {
		for i := range wantTags {
			if got[i] != wantTags[i] {
				t.Fatalf("tags[%d] = %q, want %q", i, got[i], wantTags[i])
			}
		}
	}

	if len(remoteTags) != len(wantTags) {
		t.Fatalf("remote tags len = %d, want %d (%v)", len(remoteTags), len(wantTags), remoteTags)
	}
	for i := range wantTags {
		if remoteTags[i] != wantTags[i] {
			t.Fatalf("remoteTags[%d] = %q, want %q", i, remoteTags[i], wantTags[i])
		}
	}
}

func TestRunEditRemovesCanonicalizedTag(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobTags(database, jobID, []string{"rental", "exp-012"}); err != nil {
		t.Fatalf("set initial tags: %v", err)
	}

	resetEditState()
	cmd := newEditTestCommand()
	if err := cmd.Flags().Set("remove-tag", "cloud"); err != nil {
		t.Fatalf("set remove-tag flag: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runEdit(cmd, []string{fmt.Sprintf("%d", jobID)}); err != nil {
			t.Fatalf("runEdit: %v", err)
		}
	})
	if !strings.Contains(out, "tags: exp-012") {
		t.Fatalf("output missing tag update, got %q", out)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got := job.Tags; len(got) != 1 || got[0] != "exp-012" {
		t.Fatalf("tags = %v, want [exp-012]", got)
	}
}

func TestRunEditUpdatesGPUMem(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	resetEditState()
	cmd := newEditTestCommand()
	if err := cmd.Flags().Set("gpu-mem", "48"); err != nil {
		t.Fatalf("set gpu-mem flag: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runEdit(cmd, []string{fmt.Sprintf("%d", jobID)}); err != nil {
			t.Fatalf("runEdit: %v", err)
		}
	})
	if !strings.Contains(out, "GPU memory: 48 GB") {
		t.Fatalf("output missing gpu mem update, got %q", out)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 48 {
		t.Fatalf("GPUMemGB = %v, want 48", job.GPUMemGB)
	}
}

func TestRunEditUpdatesGPUClassOverride(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, "4090"); err != nil {
		t.Fatalf("set initial gpu class: %v", err)
	}
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{
		GPUClass: "4090",
	}); err != nil {
		t.Fatalf("set cli overrides: %v", err)
	}

	resetEditState()
	cmd := newEditTestCommand()
	if err := cmd.Flags().Set("gpu-class", "ampere+"); err != nil {
		t.Fatalf("set gpu-class flag: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runEdit(cmd, []string{fmt.Sprintf("%d", jobID)}); err != nil {
			t.Fatalf("runEdit: %v", err)
		}
	})
	if !strings.Contains(out, "GPU class: ampere+") {
		t.Fatalf("output missing gpu class update, got %q", out)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUClass != "ampere+" {
		t.Fatalf("GPUClass = %q, want ampere+", job.GPUClass)
	}
	if job.CLIResourceOverrides == nil || job.CLIResourceOverrides.GPUClass != "ampere+" {
		t.Fatalf("CLI GPUClass override = %+v, want ampere+", job.CLIResourceOverrides)
	}
}

func TestRunEditRejectsTagAndClearTags(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	resetEditState()
	cmd := newEditTestCommand()
	if err := cmd.Flags().Set("tag", "benchmark"); err != nil {
		t.Fatalf("set tag flag: %v", err)
	}
	if err := cmd.Flags().Set("clear-tags", "true"); err != nil {
		t.Fatalf("set clear-tags flag: %v", err)
	}

	err = runEdit(cmd, []string{fmt.Sprintf("%d", jobID)})
	if err == nil || !errors.Is(err, errFlagConflict) {
		t.Fatalf("expected errFlagConflict, got %v", err)
	}
}

func TestRunEditRejectsRemoveTagConflicts(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	for _, tc := range []struct {
		name string
		set  func(*cobra.Command)
	}{
		{
			name: "tag",
			set: func(cmd *cobra.Command) {
				if err := cmd.Flags().Set("tag", "exp-012"); err != nil {
					t.Fatalf("set tag flag: %v", err)
				}
			},
		},
		{
			name: "clear-tags",
			set: func(cmd *cobra.Command) {
				if err := cmd.Flags().Set("clear-tags", "true"); err != nil {
					t.Fatalf("set clear-tags flag: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetEditState()
			cmd := newEditTestCommand()
			if err := cmd.Flags().Set("remove-tag", "benchmark"); err != nil {
				t.Fatalf("set remove-tag flag: %v", err)
			}
			tc.set(cmd)

			err := runEdit(cmd, []string{fmt.Sprintf("%d", jobID)})
			if err == nil || !errors.Is(err, errFlagConflict) {
				t.Fatalf("expected errFlagConflict, got %v", err)
			}
		})
	}
}

func resetEditState() {
	queueEditDepends = nil
	queueEditDependsAny = nil
	queueEditClearDeps = false
	editMessage = ""
	editProject = ""
	editCommand = ""
	editDirectory = ""
	editEnvVars = nil
	editClearEnv = false
	editTags = nil
	editRemoveTags = nil
	editClearTags = false
	editStatus = ""
	editRetry = false
	editGPUClass = ""
	editGPUMem = 0
	editProvider = ""
	editInputs = nil
	editClearInputs = false
}

func newEditTestCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "edit <job-id>"}
	addEditFlags(cmd)
	return cmd
}

func extractQueuedJobTags(command string, jobID int64) ([]string, error) {
	const marker = "printf '%s\\n' "
	suffix := fmt.Sprintf(" > ~/.cache/weft/queue/job-%d.json", jobID)

	start := strings.Index(command, marker)
	if start == -1 {
		return nil, fmt.Errorf("queue write command missing printf marker")
	}
	rest := command[start+len(marker):]
	end := strings.Index(rest, suffix)
	if end == -1 {
		return nil, fmt.Errorf("queue write command missing job file suffix")
	}

	payloadJSON, err := strconv.Unquote(strings.TrimSpace(rest[:end]))
	if err != nil {
		return nil, fmt.Errorf("unquote queue payload: %w", err)
	}

	var payload struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return nil, fmt.Errorf("decode queue payload: %w", err)
	}
	return payload.Tags, nil
}
