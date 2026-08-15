package cloud

import (
	"slices"
	"testing"
)

func TestInstanceSSHArgsUsesConfiguredIdentity(t *testing.T) {
	t.Cleanup(func() { SetSSHIdentityFile("") })
	SetSSHIdentityFile("/tmp/weft_cloud_ed25519")

	args := InstanceSSHArgs(&Instance{SSHPort: 2222})
	for _, want := range []string{"BatchMode=yes", "IdentitiesOnly=yes", "/tmp/weft_cloud_ed25519"} {
		if !slices.Contains(args, want) {
			t.Fatalf("InstanceSSHArgs() = %#v, missing %q", args, want)
		}
	}
}

func TestInstanceSSHArgsEnforcesNonInteractiveTrustBoundary(t *testing.T) {
	t.Cleanup(func() { SetSSHIdentityFile("") })
	args := InstanceSSHArgs(&Instance{SSHPort: 2222})
	for _, want := range []string{
		"-F", "/dev/null",
		"BatchMode=yes",
		"StrictHostKeyChecking=no",
		"UserKnownHostsFile=/dev/null",
		"LogLevel=ERROR",
		"IdentitiesOnly=yes",
		"IdentityAgent=none",
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("InstanceSSHArgs() = %#v, missing %q", args, want)
		}
	}
}

func TestInstanceSSHArgsFormatsPortAsLiteralValue(t *testing.T) {
	t.Cleanup(func() { SetSSHIdentityFile("") })
	args := InstanceSSHArgs(&Instance{SSHPort: 2222})
	// -p must be followed by the decimal port; no other values are injected.
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			if args[i+1] != "2222" {
				t.Fatalf("InstanceSSHArgs() port = %q, want 2222", args[i+1])
			}
			return
		}
	}
	t.Fatalf("InstanceSSHArgs() = %#v, missing -p 2222", args)
}

func TestInstanceSSHTargetFormatsRootAtHost(t *testing.T) {
	got := InstanceSSHTarget(&Instance{SSHHost: "1.2.3.4"})
	if got != "root@1.2.3.4" {
		t.Fatalf("InstanceSSHTarget() = %q, want root@1.2.3.4", got)
	}
}
