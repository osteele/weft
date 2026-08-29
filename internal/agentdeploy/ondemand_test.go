package agentdeploy

import (
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
