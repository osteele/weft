package campaign

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestSourceInputsByDir(t *testing.T) {
	root := t.TempDir()
	workdirA := filepath.Join(root, "proj-a")
	workdirB := filepath.Join(root, "proj-b")

	jobs := []*db.Job{
		{WorkingDir: workdirA, Inputs: []string{"hf:gpt2", "local:data/conllu/"}},
		{WorkingDir: workdirA, Inputs: []string{"local:data/conllu/", "local:extra/"}},
		{WorkingDir: workdirB, Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
		{WorkingDir: "/workspace/proj", Inputs: []string{"local:ignored/"}}, // container path
		{WorkingDir: "", Inputs: []string{"local:ignored/"}},
		nil,
	}

	got := SourceInputsByDir(jobs)

	want := map[string][]string{
		workdirA: []string{"hf:gpt2", "local:data/conllu/", "local:extra/"},
		workdirB: []string{"hf:meta-llama/Llama-3-8B"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SourceInputsByDir() = %#v, want %#v", got, want)
	}
}
