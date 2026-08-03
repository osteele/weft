package artifacts

import (
	"errors"
	"testing"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

// stubSSH replaces the SSH runner with one returning fixed output for the
// duration of the test.
func stubSSH(t *testing.T, stdout, stderr string, err error) {
	t.Helper()
	t.Cleanup(ssh.SetRunner(func(string, string) (string, string, error) {
		return stdout, stderr, err
	}))
}

// Regression: an SSH transport failure must NOT read as ErrManifestMissing —
// `weft artifact add` overwrites the remote manifest on confirmed absence.
func TestFetchManifest_TransportErrorIsNotMissing(t *testing.T) {
	transportErr := errors.New("context deadline exceeded")
	stubSSH(t, "", "", transportErr)

	_, err := FetchManifest("host-alpha", 42, time.Second)
	if errors.Is(err, ErrManifestMissing) {
		t.Fatalf("FetchManifest() = ErrManifestMissing for a transport failure; want the transport error, got %v", err)
	}
	if !errors.Is(err, transportErr) {
		t.Fatalf("FetchManifest() error = %v, want wrapped %v", err, transportErr)
	}
}

func TestFetchManifest_SentinelMeansConfirmedMissing(t *testing.T) {
	stubSSH(t, manifestMissingSentinel+"\n", "", nil)

	_, err := FetchManifest("host-alpha", 42, time.Second)
	if !errors.Is(err, ErrManifestMissing) {
		t.Fatalf("FetchManifest() error = %v, want ErrManifestMissing", err)
	}
}

func TestFetchManifest_ParsesPresentManifest(t *testing.T) {
	stubSSH(t, `{"job_id": 42, "artifacts": [{"path": "output/model.pt"}]}`, "", nil)

	manifest, err := FetchManifest("host-alpha", 42, time.Second)
	if err != nil {
		t.Fatalf("FetchManifest() error = %v", err)
	}
	if len(manifest.Artifacts) != 1 || manifest.Artifacts[0].Path != "output/model.pt" {
		t.Fatalf("FetchManifest() = %+v, want one artifact output/model.pt", manifest)
	}
}
