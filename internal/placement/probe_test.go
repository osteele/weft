package placement

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/osteele/weft/internal/ssh"
	_ "modernc.org/sqlite"
)

func TestProbeHosts(t *testing.T) {
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		switch host {
		case "cool30", "cool100":
			return "", "", nil
		default:
			return "", "", fmt.Errorf("connection refused")
		}
	})
	defer cleanup()

	result := ProbeHosts([]string{"cool30", "cool100", "studio"}, 5*time.Second)

	if !result["cool30"] {
		t.Error("cool30 should be reachable")
	}
	if !result["cool100"] {
		t.Error("cool100 should be reachable")
	}
	if result["studio"] {
		t.Error("studio should be unreachable")
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
	db := setupTestDB(t)

	// cool30 and cool100 are online; studio is offline
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		switch host {
		case "cool30", "cool100":
			return "", "", nil
		default:
			return "", "", fmt.Errorf("connection refused")
		}
	})
	defer cleanup()

	host, _, err := BestReachableHost(db, Constraints{}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Should pick one of the online hosts (cool30 or cool100)
	if host != "cool30" && host != "cool100" {
		t.Errorf("expected cool30 or cool100, got %s", host)
	}
}

func TestBestReachableHost_SkipsOfflineBest(t *testing.T) {
	db := setupTestDB(t)

	// Get scores to find the best-scored host, then make it offline
	scores, err := ScoreHosts(db, Constraints{})
	if err != nil {
		t.Fatal(err)
	}

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

	host, _, err := BestReachableHost(db, Constraints{}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if host == bestHost {
		t.Errorf("should not pick offline host %s", bestHost)
	}
	if host != secondHost {
		t.Errorf("expected second-best host %s, got %s", secondHost, host)
	}
}

func TestBestReachableHost_AllOffline(t *testing.T) {
	db := setupTestDB(t)

	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		return "", "", fmt.Errorf("connection refused")
	})
	defer cleanup()

	_, _, err := BestReachableHost(db, Constraints{}, 5*time.Second)
	if !errors.Is(err, ErrNoReachableHost) {
		t.Errorf("expected ErrNoReachableHost, got %v", err)
	}
}

func TestBestReachableHost_GPUConstraint(t *testing.T) {
	db := setupTestDB(t)

	// Require A100 — only cool100 is eligible. Make cool100 offline.
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		return "", "", fmt.Errorf("connection refused")
	})
	defer cleanup()

	_, _, err := BestReachableHost(db, Constraints{GPUClass: "a100"}, 5*time.Second)
	if !errors.Is(err, ErrNoReachableHost) {
		t.Errorf("expected ErrNoReachableHost when only eligible host is offline, got %v", err)
	}
}
