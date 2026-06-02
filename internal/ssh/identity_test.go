package ssh

import (
	"slices"
	"testing"
)

// TestIdentityArgs_EmptyByDefault: when no identity is configured, no
// `-i`/`-F` flags are injected — weft falls back to ssh's default
// behavior (which honors ~/.ssh/config and ssh-agent).
func TestIdentityArgs_EmptyByDefault(t *testing.T) {
	prev := sshIdentityFile
	t.Cleanup(func() { sshIdentityFile = prev })
	sshIdentityFile = ""
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
	prev := sshIdentityFile
	prevUsers := sshUserByHost
	t.Cleanup(func() {
		sshIdentityFile = prev
		sshUserByHost = prevUsers
	})
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
	prev := sshIdentityFile
	prevUsers := sshUserByHost
	t.Cleanup(func() {
		sshIdentityFile = prev
		sshUserByHost = prevUsers
	})
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
	prevID := sshIdentityFile
	prevUsers := sshUserByHost
	prevByHost := sshIdentityByHost
	t.Cleanup(func() {
		sshIdentityFile = prevID
		sshUserByHost = prevUsers
		sshIdentityByHost = prevByHost
	})
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
	prevID := sshIdentityFile
	prevUsers := sshUserByHost
	prevByHost := sshIdentityByHost
	t.Cleanup(func() {
		sshIdentityFile = prevID
		sshUserByHost = prevUsers
		sshIdentityByHost = prevByHost
	})
	sshIdentityFile = ""
	sshUserByHost = map[string]string{"studio": "agent"}
	sshIdentityByHost = map[string]string{"studio": "/path/to/agent_studio_key"}

	got := identityArgs("studio")
	want := []string{"-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-i", "/path/to/agent_studio_key"}
	if !slices.Equal(got, want) {
		t.Fatalf("identityArgs = %v, want %v", got, want)
	}
}

// TestHostTarget_NoOverride: with no per-host user override the host
// is returned unchanged so ssh resolves the user via ~/.ssh/config or
// the local username.
func TestHostTarget_NoOverride(t *testing.T) {
	prev := sshUserByHost
	t.Cleanup(func() { sshUserByHost = prev })
	sshUserByHost = nil
	if got := hostTarget("studio"); got != "studio" {
		t.Fatalf("hostTarget = %q, want %q", got, "studio")
	}
}

// TestHostTarget_WithUser: explicit ssh_user produces user@host so
// weft connects as a service account distinct from the user's
// interactive `ssh studio` login.
func TestHostTarget_WithUser(t *testing.T) {
	prev := sshUserByHost
	t.Cleanup(func() { sshUserByHost = prev })
	sshUserByHost = map[string]string{"studio": "agent"}
	if got := hostTarget("studio"); got != "agent@studio" {
		t.Fatalf("hostTarget = %q, want %q", got, "agent@studio")
	}
	// Other hosts unaffected.
	if got := hostTarget("cool30"); got != "cool30" {
		t.Fatalf("hostTarget for unconfigured = %q, want %q", got, "cool30")
	}
}
