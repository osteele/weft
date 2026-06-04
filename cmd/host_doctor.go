package cmd

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/osteele/weft/internal/agentenv"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
)

var hostDoctorCmd = &cobra.Command{
	Use:   "doctor <host>",
	Short: "Check whether a host is ready for the weft agent",
	Long: `Check the SSH user and agent runtime environment for tools required by
the on-prem queue runner. The report includes the raw SSH PATH and the PATH
that weft gives the agent, so hidden package-manager paths are visible.`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runHostDoctor,
}

func runHostDoctor(cmd *cobra.Command, args []string) error {
	return runHostDoctorWithRunner(cmd.OutOrStdout(), args[0], ssh.Run)
}

func runHostDoctorWithRunner(w io.Writer, host string, run ssh.RunnerFunc) error {
	rawOut, rawErr, err := run(host, hostDoctorProbeCommand(false))
	if err != nil {
		return fmt.Errorf("probe raw SSH environment: %w\n%s", err, rawErr)
	}
	agentOut, agentErr, err := run(host, hostDoctorProbeCommand(true))
	if err != nil {
		return fmt.Errorf("probe agent environment: %w\n%s", err, agentErr)
	}

	fmt.Fprintf(w, "Host: %s\n\n", host)
	fmt.Fprintln(w, "Raw SSH environment:")
	writeDoctorProbe(w, rawOut)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Weft agent environment:")
	writeDoctorProbe(w, agentOut)
	return nil
}

func hostDoctorProbeCommand(agentPath bool) string {
	probe := `printf 'user	%s
' "$(id -un)"
printf 'home	%s
' "$HOME"
printf 'path	%s
' "$PATH"
for cmd in tmux jq rsync rclone curl unzip uv brew; do
  if path="$(command -v "$cmd" 2>/dev/null)"; then
    printf 'tool	%s	%s
' "$cmd" "$path"
  else
    printf 'missing	%s
' "$cmd"
  fi
done
for brew in /opt/homebrew/bin/brew /usr/local/bin/brew; do
  if [ -x "$brew" ]; then
    prefix="${brew%/bin/brew}"
    if [ -w "$prefix" ] && [ -w "$prefix/bin" ]; then writable=yes; else writable=no; fi
    printf 'package_manager	brew	%s	writable=%s
' "$brew" "$writable"
  fi
done`
	if agentPath {
		return agentenv.ShellPrefix(probe)
	}
	return probe
}

func writeDoctorProbe(w io.Writer, probe string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	for _, line := range strings.Split(strings.TrimSpace(probe), "\n") {
		fields := strings.Split(line, "\t")
		switch fields[0] {
		case "user":
			fmt.Fprintf(tw, "  user\t%s\n", valueAt(fields, 1))
		case "home":
			fmt.Fprintf(tw, "  home\t%s\n", valueAt(fields, 1))
		case "path":
			fmt.Fprintf(tw, "  PATH\t%s\n", valueAt(fields, 1))
		case "tool":
			fmt.Fprintf(tw, "  ok\t%s\t%s\n", valueAt(fields, 1), valueAt(fields, 2))
		case "missing":
			fmt.Fprintf(tw, "  missing\t%s\n", valueAt(fields, 1))
		case "package_manager":
			fmt.Fprintf(tw, "  package-manager\t%s\t%s\t%s\n", valueAt(fields, 1), valueAt(fields, 2), valueAt(fields, 3))
		}
	}
}

func valueAt(values []string, index int) string {
	if index >= len(values) {
		return ""
	}
	return values[index]
}
