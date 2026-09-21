package agentdeploy

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/config"
)

func TestSSHBuilderPinsFileIdentityWithoutAgent(t *testing.T) {
	builder := config.AgentBuilder{
		Host:         "aws-build-server",
		IdentityFile: "/tmp/key with space",
	}

	args := sshBaseArgs(builder)
	for _, want := range []string{
		"IdentityAgent=none", "IdentitiesOnly=yes", "/tmp/key with space", "aws-build-server",
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("sshBaseArgs() = %v, missing %q", args, want)
		}
	}

	rsyncCommand := rsyncSSHCommand(builder)
	for _, want := range []string{
		"IdentityAgent=none", "IdentitiesOnly=yes", "'/tmp/key with space'",
	} {
		if !strings.Contains(rsyncCommand, want) {
			t.Fatalf("rsyncSSHCommand() = %q, missing %q", rsyncCommand, want)
		}
	}
}

// stubRepoRoot points the package's repo-root seam at dir for the duration of
// the test, so resolveBuilders' repo env file lookup is hermetic.
func stubRepoRoot(t *testing.T, dir string, err error) {
	t.Helper()
	orig := repoRootFunc
	repoRootFunc = func() (string, error) { return dir, err }
	t.Cleanup(func() { repoRootFunc = orig })
}

// Installs from a jj workspace and the daemon have no repo .envrc beside
// them. Builder configuration must still resolve from the config-dir env
// file, or exactly those processes see zero builders.
func TestResolveBuildersFallsBackToConfigDirEnvFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, key := range []string{
		"WEFT_LINUX_BUILDER_HOST", "WEFT_FLY_BUILDER_APP", "WEFT_FLY_BUILDER_MACHINE",
	} {
		t.Setenv(key, "")
	}
	stubRepoRoot(t, "", fmt.Errorf("no source tree"))

	dir := filepath.Join(home, ".config", "weft")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := "WEFT_FLY_BUILDER_APP=config-app\nexport WEFT_FLY_BUILDER_MACHINE=config-machine\n"
	if err := os.WriteFile(filepath.Join(dir, "fly-builder.env"), []byte(envFile), 0o600); err != nil {
		t.Fatal(err)
	}

	builders, err := resolveBuilders("linux", "amd64")
	if err != nil {
		t.Fatalf("resolveBuilders: %v", err)
	}
	if len(builders) != 1 {
		t.Fatalf("resolveBuilders returned %d builders, want 1: %+v", len(builders), builders)
	}
	b := builders[0]
	if b.Type != "fly" || b.App != "config-app" || b.Machine != "config-machine" {
		t.Fatalf("builder = %+v, want fly config-app/config-machine", b)
	}
}

// When several sources define the same builder variable, the process
// environment wins over the repo env file, which wins over the config-dir env
// file.
func TestResolveBuildersEnvSourcePrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, key := range []string{
		"WEFT_LINUX_BUILDER_HOST", "WEFT_FLY_BUILDER_APP", "WEFT_FLY_BUILDER_MACHINE",
	} {
		t.Setenv(key, "")
	}

	repo := t.TempDir()
	stubRepoRoot(t, repo, nil)
	repoEnv := "WEFT_FLY_BUILDER_APP=repo-app\nWEFT_FLY_BUILDER_MACHINE=repo-machine\n"
	if err := os.WriteFile(filepath.Join(repo, ".envrc"), []byte(repoEnv), 0o600); err != nil {
		t.Fatal(err)
	}

	configDir := filepath.Join(home, ".config", "weft")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configEnv := "WEFT_FLY_BUILDER_APP=config-app\nWEFT_FLY_BUILDER_MACHINE=config-machine\n"
	if err := os.WriteFile(filepath.Join(configDir, "fly-builder.env"), []byte(configEnv), 0o600); err != nil {
		t.Fatal(err)
	}

	flyApp := func() string {
		builders, err := resolveBuilders("linux", "amd64")
		if err != nil {
			t.Fatalf("resolveBuilders: %v", err)
		}
		for _, b := range builders {
			if b.Type == "fly" {
				return b.App
			}
		}
		t.Fatalf("no fly builder in %+v", builders)
		return ""
	}

	if got := flyApp(); got != "repo-app" {
		t.Fatalf("repo env file should beat config-dir env file, got app %q", got)
	}
	t.Setenv("WEFT_FLY_BUILDER_APP", "env-app")
	if got := flyApp(); got != "env-app" {
		t.Fatalf("process env should beat repo env file, got app %q", got)
	}
}

func TestNormalizeSSHBuilderUsesDefaultIdentity(t *testing.T) {
	builders := normalizeBuilders([]config.AgentBuilder{{
		Type: "ssh", Host: "aws-build-server",
	}}, "/tmp/cloud-key")
	if len(builders) != 1 {
		t.Fatalf("normalizeBuilders() returned %d builders, want 1", len(builders))
	}
	if builders[0].IdentityFile != "/tmp/cloud-key" {
		t.Fatalf("IdentityFile = %q, want /tmp/cloud-key", builders[0].IdentityFile)
	}
}
