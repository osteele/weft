package dataplane

import "testing"

func TestSourceTarball(t *testing.T) {
	if got := SourceTarball("abc123"); got != "sources/abc123.tar.gz" {
		t.Fatalf("SourceTarball = %q", got)
	}
}

func TestJobAttemptArtifactManifest(t *testing.T) {
	if got := JobAttemptArtifactManifest(17, 23); got != "jobs/17/runs/23/artifacts/manifest.json" {
		t.Fatalf("JobAttemptArtifactManifest = %q", got)
	}
}

func TestJobAttemptPublicationReport(t *testing.T) {
	if got := JobAttemptPublicationReport(17, 23); got != "jobs/17/runs/23/publication.json" {
		t.Fatalf("JobAttemptPublicationReport = %q", got)
	}
}
