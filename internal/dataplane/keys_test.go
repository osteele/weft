package dataplane

import "testing"

func TestSourceTarball(t *testing.T) {
	if got := SourceTarball("abc123"); got != "sources/abc123.tar.gz" {
		t.Fatalf("SourceTarball = %q", got)
	}
	if got := SourceTarballV2("abc123"); got != "sources/v2/sha256/abc123.tar.gz" {
		t.Fatalf("SourceTarballV2 = %q", got)
	}
}

func TestSourceClosureReceipt(t *testing.T) {
	if got := SourceClosureReceipt("abc123"); got != "source-closures/v1/abc123.json" {
		t.Fatalf("SourceClosureReceipt = %q", got)
	}
	if got := SourceClosureReceiptV2("abc123"); got != "source-closures/v2/sha256/abc123.json" {
		t.Fatalf("SourceClosureReceiptV2 = %q", got)
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
