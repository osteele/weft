package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestHostDoctorProbeCommandAgentPath(t *testing.T) {
	cmd := hostDoctorProbeCommand(true)
	for _, want := range []string{
		`export PATH="$HOME/.cache/weft/bin:$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/local/sbin:$PATH"`,
		`command -v "$cmd"`,
		`/opt/homebrew/bin/brew`,
		`writable=`,
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("hostDoctorProbeCommand(true) missing %q in:\n%s", want, cmd)
		}
	}
}

func TestRunHostDoctorReportsRawAndAgentEnvironment(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	err := runHostDoctorWithRunner(&out, "studio", func(host, command string) (string, string, error) {
		calls++
		if host != "studio" {
			t.Fatalf("host = %q, want studio", host)
		}
		if calls == 1 && strings.HasPrefix(command, "export PATH=") {
			t.Fatal("raw probe unexpectedly exports agent PATH")
		}
		if calls == 2 && !strings.HasPrefix(command, "export PATH=") {
			t.Fatal("agent probe missing agent PATH prefix")
		}
		return "user\tagent\nhome\t/Users/agent\npath\t/usr/bin:/bin\ntool\tjq\t/usr/bin/jq\nmissing\trclone\npackage_manager\tbrew\t/opt/homebrew/bin/brew\twritable=no\n", "", nil
	})
	if err != nil {
		t.Fatalf("runHostDoctorWithRunner: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	got := out.String()
	for _, want := range []string{
		"Raw SSH environment:",
		"Weft agent environment:",
		"missing",
		"rclone",
		"package-manager",
		"writable=no",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}
