package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestMineAnomaliesFlagsHFMetadataGaps(t *testing.T) {
	exitCode := 1
	rec := normalizeJobRecord(&db.Job{
		ID:       42,
		Status:   db.StatusCompleted,
		ExitCode: &exitCode,
		Inputs:   []string{"hf:gpt2"},
	}, nil)

	got := mineAnomalies([]*normalizedJobRecord{rec})
	if len(got) != 1 {
		t.Fatalf("anomalies = %#v, want one", got)
	}
	reasons := strings.Join(got[0].Reasons, "|")
	for _, want := range []string{"exit code 1", "HF inputs without explicit HF_HUB_OFFLINE override", "HF inputs without explicit HF_HOME"} {
		if !strings.Contains(reasons, want) {
			t.Fatalf("reasons missing %q: %v", want, got[0].Reasons)
		}
	}
}

func TestMineChurnGroupsScriptIterations(t *testing.T) {
	recs := []*normalizedJobRecord{
		normalizeJobRecord(&db.Job{ID: 10, Project: "proj", Command: "uv run python scripts/train.py --seed 1", EnvVars: []string{"HF_HOME=/a"}}, nil),
		normalizeJobRecord(&db.Job{ID: 11, Project: "proj", Command: "uv run python scripts/train.py --seed 2", EnvVars: []string{"HF_HOME=/b"}}, nil),
		normalizeJobRecord(&db.Job{ID: 12, Project: "proj", Command: "uv run python scripts/train.py --seed 3", EnvVars: []string{"HF_HOME=/b"}}, nil),
	}

	groups := mineChurn(recs)
	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want one", groups)
	}
	if !strings.Contains(groups[0].Key, "scripts/train.py") {
		t.Fatalf("key = %q, want script path", groups[0].Key)
	}
	if !slicesContains(groups[0].Why, "env changed") {
		t.Fatalf("why = %v, want env changed", groups[0].Why)
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
