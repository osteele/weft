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
