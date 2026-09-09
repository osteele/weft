package agentdeploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/inventory"
)

func restoreCLITestFuncs() func() {
	originalLocal := localCLIVersionFunc
	originalRemote := remoteCLIVersionFunc
	originalBuild := buildCLIOnHostFunc
	originalSSH := sshRunFunc
	return func() {
		localCLIVersionFunc = originalLocal
		remoteCLIVersionFunc = originalRemote
		buildCLIOnHostFunc = originalBuild
		sshRunFunc = originalSSH
	}
}

func TestRemoteCLIVersionDistinguishesAbsenceFailureAndVersion(t *testing.T) {
	defer restoreCLITestFuncs()()

	tests := []struct {
		name    string
		stdout  string
		stderr  string
		runErr  error
		want    string
		wantErr string
	}{
		{name: "absent", stdout: "ABSENT\n"},
		{name: "unrunnable", stdout: "UNRUNNABLE\n", want: "UNRUNNABLE"},
		{name: "version", stdout: "weft abc123def456\n", want: "abc123def456"},
		{name: "malformed", stdout: "abc123def456\n", wantErr: "unexpected output"},
		{name: "unknown", stderr: "network down", runErr: errors.New("exit status 255"), wantErr: "network down"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sshRunFunc = func(string, string) (string, string, error) {
				return tc.stdout, tc.stderr, tc.runErr
			}
			got, err := RemoteCLIVersion("studio")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if got != tc.want {
					t.Fatalf("version = %q, want %q", got, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestEnsureCLIOnHostBuildsNativeCLIAndVerifiesVersion(t *testing.T) {
	defer restoreCLITestFuncs()()

	localCLIVersionFunc = func(string, string) (string, error) { return "abc123def456", nil }
	remoteCalls := 0
	remoteCLIVersionFunc = func(host string) (string, error) {
		remoteCalls++
		if remoteCalls == 1 {
			return "old", nil
		}
		return "abc123def456", nil
	}
	var builtHost, builtVersion, builtOS, builtArch string
	buildCLIOnHostFunc = func(host, version, goos, goarch string) error {
		builtHost, builtVersion, builtOS, builtArch = host, version, goos, goarch
		return nil
	}

	changed, err := EnsureCLIOnHost("cool30", inventory.HostSpec{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("stale CLI was not replaced")
	}
	if builtHost != "cool30" || builtVersion != "abc123def456" || builtOS != "linux" || builtArch != "amd64" {
		t.Fatalf("native build = %q %q %q/%q", builtHost, builtVersion, builtOS, builtArch)
	}
	if remoteCalls != 2 {
		t.Fatalf("remote version checked %d times, want before and after build", remoteCalls)
	}
}

func TestEnsureCLIOnHostSkipsMatchingVersion(t *testing.T) {
	defer restoreCLITestFuncs()()

	localCLIVersionFunc = func(string, string) (string, error) { return "abc123def456", nil }
	remoteCLIVersionFunc = func(string) (string, error) { return "abc123def456", nil }
	buildCLIOnHostFunc = func(string, string, string, string) error {
		t.Fatal("matching CLI triggered a native build")
		return nil
	}

	changed, err := EnsureCLIOnHost("studio", inventory.HostSpec{OS: "darwin", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("matching CLI reported a change")
	}
}

func TestEnsureCLIOnHostRejectsUnverifiedBuild(t *testing.T) {
	defer restoreCLITestFuncs()()

	localCLIVersionFunc = func(string, string) (string, error) { return "abc123def456", nil }
	remoteCalls := 0
	remoteCLIVersionFunc = func(string) (string, error) {
		remoteCalls++
		if remoteCalls == 1 {
			return "old", nil
		}
		return "wrong", nil
	}
	buildCLIOnHostFunc = func(string, string, string, string) error { return nil }

	changed, err := EnsureCLIOnHost("studio", inventory.HostSpec{OS: "darwin", Arch: "arm64"})
	if !changed || err == nil || !strings.Contains(err.Error(), "version mismatch") {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}

func TestLocalCLIVersionHashFormatAndStability(t *testing.T) {
	version1, err := LocalCLIVersion("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	version2, err := LocalCLIVersion("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if version1 != version2 {
		t.Fatalf("CLI source version changed between calls: %q != %q", version1, version2)
	}
	if len(version1) != 12 {
		t.Fatalf("CLI source version = %q, want 12 hex characters", version1)
	}
	for _, char := range version1 {
		if !strings.ContainsRune("0123456789abcdef", char) {
			t.Fatalf("CLI source version = %q, contains non-hex %q", version1, char)
		}
	}
}

func TestLocalCLIVersionUsesTargetPlatformFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"go.mod":           "module example.com/targethash\n\ngo 1.23.0\n",
		"main.go":          "package main\n\nfunc main() {}\n",
		"target_linux.go":  "//go:build linux\n\npackage main\n\nconst targetValue = \"linux-v1\"\n",
		"target_darwin.go": "//go:build darwin\n\npackage main\n\nconst targetValue = \"darwin-v1\"\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	linuxBefore, err := localCLISourceVersion(root, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	darwinBefore, err := localCLISourceVersion(root, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "target_linux.go"),
		[]byte("//go:build linux\n\npackage main\n\nconst targetValue = \"linux-v2\"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	linuxAfter, err := localCLISourceVersion(root, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	darwinAfter, err := localCLISourceVersion(root, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if linuxAfter == linuxBefore {
		t.Fatal("Linux CLI version ignored a changed Linux-only source file")
	}
	if darwinAfter != darwinBefore {
		t.Fatalf("Darwin CLI version changed after Linux-only edit: %q != %q", darwinAfter, darwinBefore)
	}
}
