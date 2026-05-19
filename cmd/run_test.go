package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/spf13/cobra"
)

func TestParseCdPrefix(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		wantDir  string
		wantRest string
	}{
		{"unquoted with &&", "cd /path/to/dir && python train.py", "/path/to/dir", "python train.py"},
		{"unquoted with semicolon", "cd /path/to/dir; python train.py", "/path/to/dir", "python train.py"},
		{"single-quoted path", "cd '/path/to my dir' && python train.py", "/path/to my dir", "python train.py"},
		{"double-quoted path", `cd "/path/to my dir" && python train.py`, "/path/to my dir", "python train.py"},
		{"no cd prefix", "python train.py", "", "python train.py"},
		{"cd only no separator", "cd /path/to/dir", "", "cd /path/to/dir"},
		{"tilde path", "cd ~/projects && make", "~/projects", "make"},
		{"leading whitespace", "  cd /foo && bar", "/foo", "bar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, rest := parseCdPrefix(tt.command)
			if dir != tt.wantDir || rest != tt.wantRest {
				t.Errorf("parseCdPrefix(%q) = (%q, %q), want (%q, %q)",
					tt.command, dir, rest, tt.wantDir, tt.wantRest)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"safe string", "hello", "hello"},
		{"empty string", "", "''"},
		{"spaces", "hello world", "'hello world'"},
		{"special chars", "foo$bar", "'foo$bar'"},
		{"inner single quotes", "it's", "'it'\"'\"'s'"},
		{"tilde", "~/path", "'~/path'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestPathHasHomePrefix(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}

	tests := []struct {
		name string
		dir  string
		home string
		want bool
	}{
		{"exact match", home, home, true},
		{"subdirectory", filepath.Join(home, "projects"), home, true},
		{"sibling", home + "-other", home, false},
		{"unrelated", "/tmp/foo", home, false},
		{"trailing slash cleaned", home + "/", home, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pathHasHomePrefix(tt.dir, tt.home)
			if got != tt.want {
				t.Errorf("pathHasHomePrefix(%q, %q) = %v, want %v", tt.dir, tt.home, got, tt.want)
			}
		})
	}
}

func TestSubmitJobToCloudReuse_AckReceived(t *testing.T) {
	prev := submitJobsToInstanceFunc
	t.Cleanup(func() { submitJobsToInstanceFunc = prev })

	submitJobsToInstanceFunc = func(context.Context, *sql.DB, *r2.Client, int64, []*db.Job) error {
		return nil
	}

	outcome, _, err := submitJobToCloudReuse(nil, nil, 42, &db.Job{ID: 1})
	if err != nil {
		t.Fatalf("submitJobToCloudReuse: %v", err)
	}
	if outcome != cloudReuseAckReceived {
		t.Fatalf("outcome = %v, want %v", outcome, cloudReuseAckReceived)
	}
}

func TestSubmitJobToCloudReuse_AckNotObserved(t *testing.T) {
	prev := submitJobsToInstanceFunc
	t.Cleanup(func() { submitJobsToInstanceFunc = prev })

	submitJobsToInstanceFunc = func(context.Context, *sql.DB, *r2.Client, int64, []*db.Job) error {
		return context.DeadlineExceeded
	}

	outcome, _, err := submitJobToCloudReuse(nil, nil, 42, &db.Job{ID: 1})
	if err != nil {
		t.Fatalf("submitJobToCloudReuse: %v", err)
	}
	if outcome != cloudReuseAckNotObserved {
		t.Fatalf("outcome = %v, want %v", outcome, cloudReuseAckNotObserved)
	}
}

func TestSubmitJobToCloudReuse_SubmitFailure(t *testing.T) {
	prev := submitJobsToInstanceFunc
	t.Cleanup(func() { submitJobsToInstanceFunc = prev })

	wantErr := errors.New("r2 unavailable")
	submitJobsToInstanceFunc = func(context.Context, *sql.DB, *r2.Client, int64, []*db.Job) error {
		return wantErr
	}

	outcome, _, err := submitJobToCloudReuse(nil, nil, 42, &db.Job{ID: 1})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if outcome != cloudReuseSubmitFailed {
		t.Fatalf("outcome = %v, want %v", outcome, cloudReuseSubmitFailed)
	}
}

func TestScanRunScriptMetaRejectsMalformedPEP723(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte(`# /// script
# [tool.weft]
# note = "contains / and never closes
# inputs = ["hf:gpt2"]
# ///
print("hello")
`), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	_, err := scanRunScriptMeta(dir, "uv run python train.py")
	if err == nil {
		t.Fatal("expected malformed metadata to fail")
	}
	if !strings.Contains(err.Error(), "invalid script metadata") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHasEnvAssignment(t *testing.T) {
	if !hasEnvAssignment([]string{"HF_HUB_OFFLINE=0"}, "HF_HUB_OFFLINE") {
		t.Fatal("expected exact env assignment to match")
	}
	if hasEnvAssignment([]string{"MY_HF_HUB_OFFLINE=0"}, "HF_HUB_OFFLINE") {
		t.Fatal("expected prefixed env name not to match")
	}
}

func TestPersistDraftArtifactFields(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordDraftJobWithGPU(database, "cool30", "/tmp/project", "echo hello", "draft", "")
	if err != nil {
		t.Fatalf("RecordDraftJobWithGPU: %v", err)
	}

	inputs := []string{"hf-dataset:allenai/c4"}
	outputs := []string{"output/representations/model_a_train.pkl"}
	outputDirs := []string{"output/representations"}
	produces := []string{"output/representations/model_a_train.pkl"}
	needs := []string{
		"output/representations/model_b_train.pkl:1285",
		"output/representations/model_b_dev.pkl:1285",
	}

	if err := persistDraftArtifactFields(database, jobID, inputs, outputs, outputDirs, produces, needs); err != nil {
		t.Fatalf("persistDraftArtifactFields: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if !reflect.DeepEqual(job.Inputs, inputs) {
		t.Fatalf("Inputs = %v, want %v", job.Inputs, inputs)
	}
	if !reflect.DeepEqual(job.Outputs, outputs) {
		t.Fatalf("Outputs = %v, want %v", job.Outputs, outputs)
	}
	if !reflect.DeepEqual(job.OutputDirs, outputDirs) {
		t.Fatalf("OutputDirs = %v, want %v", job.OutputDirs, outputDirs)
	}
	if !reflect.DeepEqual(job.Produces, produces) {
		t.Fatalf("Produces = %v, want %v", job.Produces, produces)
	}
	if !reflect.DeepEqual(job.Needs, needs) {
		t.Fatalf("Needs = %v, want %v", job.Needs, needs)
	}
}

func TestRunDraftWithoutHostRecordsDraft(t *testing.T) {
	database := db.SetupTestDB(t)
	database.Close()

	dir := t.TempDir()
	resetRunGlobals(t)
	runDraft = true
	runDir = dir
	runDescription = "draft without host"
	runGPU = "a100>=80GB"

	cmd := newRunTestCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := runRun(cmd, []string{"python train.py"}); err != nil {
		t.Fatalf("runRun: %v\noutput:\n%s", err, out.String())
	}

	readDB, err := db.Open()
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer readDB.Close()
	jobs, err := db.ListJobsWithMaxAge(readDB, "", "", 10, 0, nil, "")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs len = %d, want 1", len(jobs))
	}
	job := jobs[0]
	if job.Status != db.StatusDraft {
		t.Fatalf("status = %q, want %q", job.Status, db.StatusDraft)
	}
	if job.Host != "" {
		t.Fatalf("host = %q, want empty", job.Host)
	}
	if job.Project != filepath.Base(dir) {
		t.Fatalf("project = %q, want %q", job.Project, filepath.Base(dir))
	}
	if job.GPUClass != "a100" {
		t.Fatalf("gpu_class = %q, want a100", job.GPUClass)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 80 {
		t.Fatalf("gpu_mem_gb = %v, want 80", job.GPUMemGB)
	}
	if !strings.Contains(out.String(), "Draft job #") {
		t.Fatalf("output missing draft confirmation:\n%s", out.String())
	}
}

func newRunTestCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("provider", "", "")
	cmd.Flags().Bool("gpu-mem-strict", false, "")
	cmd.Flags().Int("disk", 0, "")
	cmd.Flags().Int("runtime-disk", 0, "")
	return cmd
}

func resetRunGlobals(t *testing.T) {
	t.Helper()
	runHost = ""
	runDir = ""
	runDescription = ""
	runProject = ""
	runDraft = false
	runFollow = false
	runWait = false
	runNoWait = false
	runKillJobID = 0
	runFrom = 0
	runEnvVars = nil
	runTags = nil
	runAfter = 0
	runAfterAny = 0
	runGPU = ""
	runGPUMem = 0
	runGPUMemStrict = false
	runDiskGB = 0
	runRuntimeDiskGB = 0
	runGPUClass = ""
	runProvider = ""
	runInputs = nil
	runOutputs = nil
	runProduces = nil
	runNeeds = nil
	runDryRun = false
	runNoSync = false
	runHFToken = false
	runHFTokenFrom = ""
	runSecretVars = nil
}
