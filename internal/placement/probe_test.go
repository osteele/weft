package placement

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/ssh"
	_ "modernc.org/sqlite"
)

func TestProbeHosts(t *testing.T) {
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		switch host {
		case "host-beta", "host-alpha":
			return "", "", nil
		default:
			return "", "", fmt.Errorf("connection refused")
		}
	})
	defer cleanup()

	result := ProbeHosts([]string{"host-beta", "host-alpha", "host-gamma"}, 5*time.Second)

	if !result["host-beta"] {
		t.Error("host-beta should be reachable")
	}
	if !result["host-alpha"] {
		t.Error("host-alpha should be reachable")
	}
	if result["host-gamma"] {
		t.Error("host-gamma should be unreachable")
	}
}

func TestProbeHosts_Empty(t *testing.T) {
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		t.Error("should not be called for empty host list")
		return "", "", nil
	})
	defer cleanup()

	result := ProbeHosts(nil, 5*time.Second)
	if len(result) != 0 {
		t.Errorf("expected empty map, got %v", result)
	}
}

func TestBestReachableHost(t *testing.T) {
	inventory.UseTestHosts(t)
	db := setupTestDB(t)

	// host-beta and host-alpha are online; host-gamma is offline
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		switch host {
		case "host-beta", "host-alpha":
			return "", "", nil
		default:
			return "", "", fmt.Errorf("connection refused")
		}
	})
	defer cleanup()

	result, err := BestReachableHost(db, Constraints{}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-beta" && result.Host != "host-alpha" {
		t.Errorf("expected host-beta or host-alpha, got %s", result.Host)
	}
	if len(result.Scores) == 0 {
		t.Error("expected scores in PlacementResult")
	}
}

func TestBestReachableHost_SkipsOfflineBest(t *testing.T) {
	inventory.UseTestHosts(t)
	db := setupTestDB(t)

	scores := scoreTestHosts(db, Constraints{})

	// The best-scored host
	bestHost := scores[0].Host
	// The second-best eligible host
	var secondHost string
	for _, s := range scores[1:] {
		if s.Eligible {
			secondHost = s.Host
			break
		}
	}
	if secondHost == "" {
		t.Fatal("need at least 2 eligible hosts")
	}

	// Make best host offline, second host online
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		if host == bestHost {
			return "", "", fmt.Errorf("connection refused")
		}
		return "", "", nil
	})
	defer cleanup()

	result, err := BestReachableHost(db, Constraints{}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Host == bestHost {
		t.Errorf("should not pick offline host %s", bestHost)
	}
	if result.Host != secondHost {
		t.Errorf("expected second-best host %s, got %s", secondHost, result.Host)
	}
}

func TestBestReachableHost_AllOffline(t *testing.T) {
	inventory.UseTestHosts(t)
	db := setupTestDB(t)

	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		return "", "", fmt.Errorf("connection refused")
	})
	defer cleanup()

	_, err := BestReachableHost(db, Constraints{}, 5*time.Second)
	if !errors.Is(err, ErrNoReachableHost) {
		t.Errorf("expected ErrNoReachableHost, got %v", err)
	}
}

func TestBestReachableHost_GPUConstraint(t *testing.T) {
	inventory.UseTestHosts(t)
	db := setupTestDB(t)

	// Require A100 — only host-alpha is eligible. Make it offline.
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		return "", "", fmt.Errorf("connection refused")
	})
	defer cleanup()

	_, err := BestReachableHost(db, Constraints{GPUClass: "a100"}, 5*time.Second)
	if !errors.Is(err, ErrNoReachableHost) {
		t.Errorf("expected ErrNoReachableHost when only eligible host is offline, got %v", err)
	}
}
