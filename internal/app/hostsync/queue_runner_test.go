package hostsync

import (
	"errors"
	"os"
	"testing"

	"github.com/osteele/weft/internal/inventory"
)

func TestEnsureQueueRunnerStartedSurfacesAgentDeployFailure(t *testing.T) {
	originalFindHostSpec := findHostSpecFunc
	originalEnsureAgentUpToDate := ensureAgentUpToDateFunc
	t.Cleanup(func() {
		findHostSpecFunc = originalFindHostSpec
		ensureAgentUpToDateFunc = originalEnsureAgentUpToDate
	})

	findHostSpecFunc = func(host string) *inventory.HostSpec {
		return &inventory.HostSpec{Name: host, OS: "linux", Arch: "amd64"}
	}
	ensureAgentUpToDateFunc = func(host string, spec inventory.HostSpec) (bool, error) {
		return false, os.ErrPermission
	}

	started, err := EnsureQueueRunnerStarted("studio")
	if err == nil {
		t.Fatal("expected error")
	}
	if started {
		t.Fatal("started = true, want false")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected os.ErrPermission in error chain, got: %v", err)
	}
}
