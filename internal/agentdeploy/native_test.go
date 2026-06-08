package agentdeploy

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func restoreNativeTestFuncs() func() {
	origRepoRoot := repoRootFunc
	origRsyncSourcesToHost := rsyncSourcesToHostFunc
	origEnsureGoOnHost := ensureGoOnHostFunc
	origSSHRunWithTimeout := sshRunWithTimeoutFunc
	origSSHRun := sshRunFunc
	origSSHRunWithStdin := sshRunWithStdinFunc
	origLocalGoVersion := localGoVersionFunc
	return func() {
		repoRootFunc = origRepoRoot
		rsyncSourcesToHostFunc = origRsyncSourcesToHost
		ensureGoOnHostFunc = origEnsureGoOnHost
		sshRunWithTimeoutFunc = origSSHRunWithTimeout
		sshRunFunc = origSSHRun
		sshRunWithStdinFunc = origSSHRunWithStdin
		localGoVersionFunc = origLocalGoVersion
	}
}

func TestBuildOnHostWithProgressReportsNativePhases(t *testing.T) {
	defer restoreNativeTestFuncs()()

	var syncedRoot, syncedHost string
	var buildHost, buildCmd string
	var buildTimeout time.Duration
	repoRootFunc = func() (string, error) {
		return "/repo", nil
	}
	rsyncSourcesToHostFunc = func(root, host string) error {
		syncedRoot = root
		syncedHost = host
		return nil
	}
	ensureGoOnHostFunc = func(host, goos, goarch string, onProgress BuildProgressFunc) (string, error) {
		if host != "cool30" || goos != "linux" || goarch != "amd64" {
			t.Fatalf("ensureGoOnHostFunc(%q, %q, %q)", host, goos, goarch)
		}
		return "/usr/local/go/bin/go", nil
	}
	sshRunWithTimeoutFunc = func(host, cmd string, timeout time.Duration) (string, string, error) {
		buildHost = host
		buildCmd = cmd
		buildTimeout = timeout
		return "", "", nil
	}

	var phases []string
	err := BuildOnHostWithProgress("cool30", "abc123def456", "linux", "amd64", func(phase string) {
		phases = append(phases, phase)
	})
	if err != nil {
		t.Fatalf("BuildOnHostWithProgress: %v", err)
	}

	wantPhases := []string{
		"syncing source for native agent build",
		"checking Go on remote",
		"building agent on remote",
	}
	if !slices.Equal(phases, wantPhases) {
		t.Fatalf("phases = %#v, want %#v", phases, wantPhases)
	}
	if syncedRoot != "/repo" || syncedHost != "cool30" {
		t.Fatalf("synced root/host = %q/%q, want /repo/cool30", syncedRoot, syncedHost)
	}
	if buildHost != "cool30" {
		t.Fatalf("build host = %q, want cool30", buildHost)
	}
	if buildTimeout != 10*time.Minute {
		t.Fatalf("build timeout = %s, want 10m", buildTimeout)
	}
	if !strings.Contains(buildCmd, "go build") || !strings.Contains(buildCmd, "-X main.version=abc123def456") {
		t.Fatalf("build command missing expected arguments: %s", buildCmd)
	}
}

func TestEnsureGoOnHostWithProgressReportsInstallPhase(t *testing.T) {
	defer restoreNativeTestFuncs()()

	localGoVersionFunc = func() (string, error) {
		return "1.25.7", nil
	}
	sshRunFunc = func(host, cmd string) (string, string, error) {
		if host != "cool30" {
			t.Fatalf("sshRunFunc host = %q, want cool30", host)
		}
		return "", "", nil
	}
	var installHost, installCmd, installScript string
	sshRunWithStdinFunc = func(host, cmd, stdin string) (string, string, error) {
		installHost = host
		installCmd = cmd
		installScript = stdin
		return "", "", nil
	}

	var phases []string
	got, err := ensureGoOnHostWithProgress("cool30", "linux", "amd64", func(phase string) {
		phases = append(phases, phase)
	})
	if err != nil {
		t.Fatalf("ensureGoOnHostWithProgress: %v", err)
	}

	if got != remoteGoDir+"/bin/go" {
		t.Fatalf("go binary = %q, want %q", got, remoteGoDir+"/bin/go")
	}
	if !slices.Equal(phases, []string{"installing Go on remote"}) {
		t.Fatalf("phases = %#v, want installing phase", phases)
	}
	if installHost != "cool30" || installCmd != "bash -s" {
		t.Fatalf("install target = %q %q, want cool30 bash -s", installHost, installCmd)
	}
	if !strings.Contains(installScript, "VERSION=1.25.7") ||
		!strings.Contains(installScript, "go${VERSION}.linux-amd64.tar.gz") {
		t.Fatalf("install script missing Go tarball URL: %s", installScript)
	}
}
