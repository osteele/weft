package cmd

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
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
