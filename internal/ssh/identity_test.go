package ssh

import (
	"slices"
	"testing"
)

// resetIdentityGlobals zeroes the package-level identity globals and restores
// them on test cleanup. Tests should call this first so that any leaked state
// from a process-wide Configure() (or future init regression) doesn't bleed
// into the assertions — see the "agent_studio" key leak from real user config.
func resetIdentityGlobals(t *testing.T) {
	t.Helper()
	prevFile, prevUsers, prevByHost := sshIdentityFile, sshUserByHost, sshIdentityByHost
	sshIdentityFile = ""
	sshUserByHost = nil
	sshIdentityByHost = nil
	t.Cleanup(func() {
		sshIdentityFile = prevFile
		sshUserByHost = prevUsers
		sshIdentityByHost = prevByHost
	})
}

// TestIdentityArgs_EmptyByDefault: when no identity is configured, no
// `-i`/`-F` flags are injected — weft falls back to ssh's default
// behavior (which honors ~/.ssh/config and ssh-agent).
func TestIdentityArgs_EmptyByDefault(t *testing.T) {
	resetIdentityGlobals(t)
	if got := identityArgs("studio"); len(got) != 0 {
		t.Fatalf("identityArgs without identity = %v, want empty", got)
	}
}

// TestIdentityArgs_WithIdentityAndUser: identity isolation activates
// only when both an identity file is configured AND the host has an
// explicit ssh_user. The combination is the signal that weft should
// connect as a service account distinct from the user's interactive
// `ssh <host>` setup.
func TestIdentityArgs_WithIdentityAndUser(t *testing.T) {
	resetIdentityGlobals(t)
	sshIdentityFile = "/path/to/key"
	sshUserByHost = map[string]string{"studio": "agent"}
	got := identityArgs("studio")
	want := []string{"-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-i", "/path/to/key"}
	if !slices.Equal(got, want) {
		t.Fatalf("identityArgs = %v, want %v", got, want)
	}
}

// TestIdentityArgs_IdentityWithoutUserPassesThrough: when the host has
// no ssh_user override, identity isolation is NOT applied even if an
// identity file is set — ssh defaults (including ~/.ssh/config) win.
// Common case for personally-managed machines where weft shares the
// user's interactive credentials.
func TestIdentityArgs_IdentityWithoutUserPassesThrough(t *testing.T) {
	resetIdentityGlobals(t)
	sshIdentityFile = "/path/to/key"
	sshUserByHost = map[string]string{"studio": "agent"}
	if got := identityArgs("cool30"); len(got) != 0 {
		t.Fatalf("identityArgs for unconfigured host = %v, want empty (ssh.config should win)", got)
	}
}

// TestIdentityArgs_PerHostOverridesCloudFallback is a regression test for the
// "weft offered the cloud key to agent@studio, which only accepts a different
// key, so every dispatch died with EOF" bug. The fix is per-host
// ssh_identity_file taking precedence over cloud.ssh.identity_file.
func TestIdentityArgs_PerHostOverridesCloudFallback(t *testing.T) {
	resetIdentityGlobals(t)
	sshIdentityFile = "/path/to/cloud_key"
	sshUserByHost = map[string]string{"studio": "agent"}
	sshIdentityByHost = map[string]string{"studio": "/path/to/agent_studio_key"}

	got := identityArgs("studio")
	want := []string{"-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-i", "/path/to/agent_studio_key"}
	if !slices.Equal(got, want) {
		t.Fatalf("identityArgs = %v, want %v (per-host should win over cloud fallback)", got, want)
	}
}

// TestIdentityArgs_PerHostWithoutCloudFallback: a per-host identity should
// also work when there's no cluster-wide cloud key configured.
func TestIdentityArgs_PerHostWithoutCloudFallback(t *testing.T) {
	resetIdentityGlobals(t)
	sshUserByHost = map[string]string{"studio": "agent"}
	sshIdentityByHost = map[string]string{"studio": "/path/to/agent_studio_key"}

	got := identityArgs("studio")
	want := []string{"-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-i", "/path/to/agent_studio_key"}
	if !slices.Equal(got, want) {
		t.Fatalf("identityArgs = %v, want %v", got, want)
	}
}

// TestCommand_AppliesHostOverride is a regression test for log-follow and
// job-wait streaming, which need an *exec.Cmd for direct process control.
// Command() must carry user@host and IdentityAgent=none so the configured
// per-host service key is used rather than ssh falling back to ~/.ssh/config
// (the wrong user, which triggers an agent auth prompt).
func TestCommand_AppliesHostOverride(t *testing.T) {
	resetIdentityGlobals(t)
	sshUserByHost = map[string]string{"studio": "agent"}
	sshIdentityByHost = map[string]string{"studio": "/path/to/agent_studio_key"}

	cmd := Command("studio", "tail -F log")
	args := cmd.Args
	if !slices.Contains(args, "agent@studio") {
		t.Fatalf("Command args = %v, want target agent@studio", args)
	}
	if slices.Contains(args, "studio") {
		t.Fatalf("Command args = %v, must not pass bare host (bypasses override)", args)
	}
	if !slices.Contains(args, "IdentityAgent=none") || !slices.Contains(args, "/path/to/agent_studio_key") {
		t.Fatalf("Command args = %v, want IdentityAgent=none and the per-host key", args)
	}
	if !slices.Contains(args, "BatchMode=yes") {
		t.Fatalf("Command args = %v, want BatchMode=yes to prevent password prompts", args)
	}
}

// TestHostTarget_NoOverride: with no per-host user override the host
// is returned unchanged so ssh resolves the user via ~/.ssh/config or
// the local username.
func TestHostTarget_NoOverride(t *testing.T) {
	resetIdentityGlobals(t)
	if got := hostTarget("studio"); got != "studio" {
		t.Fatalf("hostTarget = %q, want %q", got, "studio")
	}
}

// TestHostTarget_WithUser: explicit ssh_user produces user@host so
// weft connects as a service account distinct from the user's
// interactive `ssh studio` login.
func TestHostTarget_WithUser(t *testing.T) {
	resetIdentityGlobals(t)
	sshUserByHost = map[string]string{"studio": "agent"}
	if got := hostTarget("studio"); got != "agent@studio" {
		t.Fatalf("hostTarget = %q, want %q", got, "agent@studio")
	}
	// Other hosts unaffected.
	if got := hostTarget("cool30"); got != "cool30" {
		t.Fatalf("hostTarget for unconfigured = %q, want %q", got, "cool30")
	}
}
