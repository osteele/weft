package cloud

import (
	"context"
	"strings"
	"testing"
)

func TestClassifyOnStartVerifyOutput(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want OnStartVerification
	}{
		{"installed", "WEFT_ONSTART_INSTALLED\n", OnStartConfirmedInstalled},
		{"missing", "WEFT_ONSTART_MISSING\n", OnStartConfirmedMissing},
		{"unreadable", "WEFT_ONSTART_UNREADABLE\n", OnStartVerificationUnknown},
		{"empty", "", OnStartVerificationUnknown},
		{"unrecognised", "bash: grep: command not found\n", OnStartVerificationUnknown},
		// A login banner or MOTD ahead of the token must not hide it.
		{"with banner", "Welcome to Ubuntu\nWEFT_ONSTART_MISSING\n", OnStartConfirmedMissing},
		// If both tokens somehow appear, the pessimistic reading wins: a
		// container reported healthy on ambiguous output is the error that
		// costs a whole rental.
		{"both tokens", "WEFT_ONSTART_INSTALLED\nWEFT_ONSTART_MISSING\n", OnStartConfirmedMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyOnStartVerifyOutput(tt.out); got != tt.want {
				t.Errorf("classifyOnStartVerifyOutput(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

// TestVerifyOnStartInstalled_NoSSHDetailsIsUnknown: an instance weft cannot
// reach must never be reported as one whose script is missing.
func TestVerifyOnStartInstalled_NoSSHDetailsIsUnknown(t *testing.T) {
	if got := VerifyOnStartInstalled(context.Background(), nil, 0); got != OnStartVerificationUnknown {
		t.Errorf("nil instance = %v, want unknown", got)
	}
	if got := VerifyOnStartInstalled(context.Background(), &Instance{Provider: ProviderVastai}, 0); got != OnStartVerificationUnknown {
		t.Errorf("instance without SSHHost = %v, want unknown", got)
	}
}

// TestSupportsOnStartVerification: only vast.ai receives weft's script as an
// OnStart command. RunPod rejects OnStartCmd and bootstraps through its
// template's dockerStartCmd, so a base-image /root/onstart.sh there carries no
// weft sentinel and would read as confirmed-missing on a healthy pod — and
// that verdict terminates with no window.
//
// Asserted on the pure predicate rather than through VerifyOnStartInstalled.
// Driving it through the SSH path with an unresolvable host would pass whether
// or not the gate existed, since a DNS failure also yields unknown.
func TestSupportsOnStartVerification(t *testing.T) {
	tests := []struct {
		name string
		inst *Instance
		want bool
	}{
		{"vastai", &Instance{Provider: ProviderVastai, SSHHost: "ssh3.vast.ai"}, true},
		{"runpod", &Instance{Provider: ProviderRunpod, SSHHost: "ssh.runpod.io"}, false},
		{"unset provider", &Instance{SSHHost: "ssh.example.com"}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SupportsOnStartVerification(tt.inst); got != tt.want {
				t.Errorf("SupportsOnStartVerification(%v) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestOnStartSentinelIsInDefaultOnStartCmd guards the coupling the verifier
// depends on: if the sentinel is ever renamed in the OnStart script without
// updating the constant, the verifier would report every healthy container as
// missing its script and terminate the fleet.
func TestOnStartSentinelIsInDefaultOnStartCmd(t *testing.T) {
	if !strings.Contains(DefaultOnStartCmd, OnStartSentinel) {
		t.Errorf("DefaultOnStartCmd does not contain OnStartSentinel %q — the verifier would confirm-missing every healthy instance", OnStartSentinel)
	}
	if !strings.Contains(R2BootstrapOnStartCmd("test-key"), OnStartSentinel) {
		t.Errorf("R2BootstrapOnStartCmd does not contain OnStartSentinel %q", OnStartSentinel)
	}
}
