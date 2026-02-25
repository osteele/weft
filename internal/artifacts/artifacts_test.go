package artifacts

import "testing"

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

func TestMergeEnvVars(t *testing.T) {
	env := []string{"CUSTOM=1", "RJ_JOB_ID=5"}
	merged := MergeEnvVars(env, 12)
	found := false
	foundManifest := false
	foundNewJobID := false
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
}
