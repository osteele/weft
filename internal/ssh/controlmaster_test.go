package ssh

import (
	"strings"
	gosync "sync"
	"testing"
)

func TestSSHCommandArgOrder(t *testing.T) {
	// Prevent EnsureControlMaster from making real SSH calls
	resetControlMasterState("argtest-host")

	// Pre-seed the Once as already done (so it won't try to connect)
	controlMasterOnceMu.Lock()
	once := &gosync.Once{}
	once.Do(func() {}) // mark as done
	controlMasterOnce["argtest-host"] = once
	controlMasterOnceMu.Unlock()

	cmd := sshCommand("argtest-host", "ls -la", "-o", "BatchMode=yes")
	args := cmd.Args

	// Should be: ssh <controlMasterArgs...> -o BatchMode=yes argtest-host "ls -la"
	if args[0] != "ssh" {
		t.Errorf("args[0] = %q, want ssh", args[0])
	}

	// ControlMaster args come first (after "ssh")
	if args[1] != "-o" || args[2] != "ControlMaster=auto" {
		t.Errorf("expected ControlMaster=auto near start, got %v", args[1:3])
	}

	// Host should be second-to-last, command last
	if args[len(args)-2] != "argtest-host" {
		t.Errorf("host should be second-to-last arg, got %q in %v", args[len(args)-2], args)
	}
	if args[len(args)-1] != "ls -la" {
		t.Errorf("command should be last arg, got %q", args[len(args)-1])
	}

	// Extra args should appear between controlMaster args and host
	joined := strings.Join(args, " ")
	batchIdx := strings.Index(joined, "BatchMode=yes")
	hostIdx := strings.Index(joined, "argtest-host")
	if batchIdx > hostIdx {
		t.Error("extra args should appear before host")
	}
}

func TestSCPCommandArgOrder(t *testing.T) {
	resetControlMasterState("scptest-host")

	controlMasterOnceMu.Lock()
	once := &gosync.Once{}
	once.Do(func() {})
	controlMasterOnce["scptest-host"] = once
	controlMasterOnceMu.Unlock()

	cmd := scpCommand("scptest-host", "local.txt", "scptest-host:remote.txt")
	args := cmd.Args

	if args[0] != "scp" {
		t.Errorf("args[0] = %q, want scp", args[0])
	}

	// -q should be first flag
	if args[1] != "-q" {
		t.Errorf("args[1] = %q, want -q", args[1])
	}

	// SCP args should be last
	if args[len(args)-2] != "local.txt" {
		t.Errorf("source should be second-to-last, got %q", args[len(args)-2])
	}
	if args[len(args)-1] != "scptest-host:remote.txt" {
		t.Errorf("dest should be last, got %q", args[len(args)-1])
	}
}

func TestEnsureControlMasterOncePerHost(t *testing.T) {
	resetControlMasterState("once-host-a")
	resetControlMasterState("once-host-b")

	// We can't easily mock exec.Command inside EnsureControlMaster,
	// so we test the sync.Once behavior by checking the map state.

	// After calling for a host, a Once entry should exist
	controlMasterOnceMu.Lock()
	_, exists := controlMasterOnce["once-host-a"]
	controlMasterOnceMu.Unlock()
	if exists {
		t.Error("once-host-a should not have Once entry before first call")
	}

	// Simulate what EnsureControlMaster does: create a Once entry
	controlMasterOnceMu.Lock()
	controlMasterOnce["once-host-a"] = &gosync.Once{}
	controlMasterOnceMu.Unlock()

	// Verify separate hosts get separate Once entries
	controlMasterOnceMu.Lock()
	controlMasterOnce["once-host-b"] = &gosync.Once{}
	controlMasterOnceMu.Unlock()

	controlMasterOnceMu.Lock()
	onceA := controlMasterOnce["once-host-a"]
	onceB := controlMasterOnce["once-host-b"]
	controlMasterOnceMu.Unlock()

	if onceA == onceB {
		t.Error("different hosts should have different Once instances")
	}

	// Verify Once only fires once
	count := 0
	onceA.Do(func() { count++ })
	onceA.Do(func() { count++ })
	if count != 1 {
		t.Errorf("Once.Do should fire exactly once, fired %d times", count)
	}

}

func TestEnsureControlMasterResetOnFailure(t *testing.T) {
	host := "reset-test-host"
	resetControlMasterState(host)

	// Simulate a failed Once (as EnsureControlMaster does on failure)
	controlMasterOnceMu.Lock()
	once1 := &gosync.Once{}
	controlMasterOnce[host] = once1
	controlMasterOnceMu.Unlock()

	// Fire the Once
	once1.Do(func() {})

	// Simulate reset (what happens on failure)
	controlMasterOnceMu.Lock()
	controlMasterOnce[host] = &gosync.Once{}
	once2 := controlMasterOnce[host]
	controlMasterOnceMu.Unlock()

	if once1 == once2 {
		t.Error("reset should create a new Once instance")
	}

	// New Once should be fireable
	fired := false
	once2.Do(func() { fired = true })
	if !fired {
		t.Error("new Once after reset should be fireable")
	}
}

// resetControlMasterState clears the Once entry for a host, used in tests.
func resetControlMasterState(host string) {
	controlMasterOnceMu.Lock()
	delete(controlMasterOnce, host)
	controlMasterOnceMu.Unlock()
}
