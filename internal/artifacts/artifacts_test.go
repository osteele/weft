package artifacts

import (
	"errors"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestParseManifest(t *testing.T) {
	content := `{"job_id":42,"artifacts":[{"name":"a","path":"out/a.txt"}]}`
	manifest, err := ParseManifest(content, 0)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if manifest.JobID != 42 {
		t.Fatalf("expected job id 42, got %d", manifest.JobID)
	}
	if len(manifest.Artifacts) != 1 || manifest.Artifacts[0].Path != "out/a.txt" {
		t.Fatalf("unexpected artifacts: %+v", manifest.Artifacts)
	}
}

func TestParseManifestFallbackJobID(t *testing.T) {
	content := `{"artifacts":[{"path":"out/a.txt"}]}`
	manifest, err := ParseManifest(content, 99)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if manifest.JobID != 99 {
		t.Fatalf("expected job id 99, got %d", manifest.JobID)
	}
}

func TestParseManifestPlainText(t *testing.T) {
	content := "data/exports/calibration_20260328.json\ndata/exports/summary.csv\n"
	manifest, err := ParseManifest(content, 42)
	if err != nil {
		t.Fatalf("ParseManifest plain text: %v", err)
	}
	if manifest.JobID != 42 {
		t.Fatalf("expected job id 42, got %d", manifest.JobID)
	}
	if len(manifest.Artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d: %+v", len(manifest.Artifacts), manifest.Artifacts)
	}
	if manifest.Artifacts[0].Path != "data/exports/calibration_20260328.json" {
		t.Fatalf("unexpected first artifact: %+v", manifest.Artifacts[0])
	}
	if manifest.Artifacts[1].Path != "data/exports/summary.csv" {
		t.Fatalf("unexpected second artifact: %+v", manifest.Artifacts[1])
	}
}

func TestParseManifestPlainTextBlankLines(t *testing.T) {
	content := "\n  output/results.json  \n\n"
	manifest, err := ParseManifest(content, 10)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(manifest.Artifacts) != 1 || manifest.Artifacts[0].Path != "output/results.json" {
		t.Fatalf("unexpected artifacts: %+v", manifest.Artifacts)
	}
}

func TestLocalRelativePath(t *testing.T) {
	cases := map[string]string{
		"output/results.json":   "output/results.json",
		"./output/results.json": "output/results.json",
		"/tmp/results.json":     "results.json",
		"../results.json":       "results.json",
		"":                      "artifact",
	}
	for input, want := range cases {
		if got := LocalRelativePath(input); got != want {
			t.Fatalf("LocalRelativePath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCopyRemoteArtifactErrorIncludesContext(t *testing.T) {
	job := &db.Job{
		ID:   2877,
		Host: "cool30",
	}
	spec := ArtifactSpec{Path: "output/exp_n06/summary.md"}
	_, _, _, err := copyRemoteArtifact(job, spec, "~/code/project/output/exp_n06/summary.md", t.TempDir(), func(_, _, _ string) error {
		return errors.New("scp failed: not a regular file")
	})
	if err == nil {
		t.Fatal("expected copy error")
	}
	msg := err.Error()
	for _, want := range []string{
		`copy artifact "output/exp_n06/summary.md"`,
		"cool30:~/code/project/output/exp_n06/summary.md",
		"scp failed: not a regular file",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

func TestMergeEnvVars(t *testing.T) {
	env := []string{"CUSTOM=1", "RJ_JOB_ID=5"}
	merged := MergeEnvVars(env, 12)
	found := false
	foundManifest := false
	foundNewJobID := false
	foundWeftJobID := false
	foundWeftManifest := false
	for _, ev := range merged {
		if ev == "RJ_JOB_ID=5" {
			found = true
		}
		if ev == "RJ_JOB_ID=12" {
			foundNewJobID = true
		}
		if ev == "RJ_ARTIFACT_MANIFEST=~/.cache/weft/artifacts/12.json" {
			foundManifest = true
		}
		if ev == "WEFT_JOB_ID=12" {
			foundWeftJobID = true
		}
		if ev == "WEFT_ARTIFACT_MANIFEST=~/.cache/weft/artifacts/12.json" {
			foundWeftManifest = true
		}
	}
	if !found {
		t.Fatalf("expected existing RJ_JOB_ID preserved")
	}
	if foundNewJobID {
		t.Fatalf("expected RJ_JOB_ID not to be overwritten")
	}
	if !foundManifest {
		t.Fatalf("expected RJ_ARTIFACT_MANIFEST to be added")
	}
	if !foundWeftJobID {
		t.Fatalf("expected WEFT_JOB_ID to be added")
	}
	if !foundWeftManifest {
		t.Fatalf("expected WEFT_ARTIFACT_MANIFEST to be added")
	}
}
