package cmd

import (
	"strings"

	"github.com/spf13/cobra"
)

var diagnoseCmd = &cobra.Command{
	Use:     "diagnose [job|instance] <id>...",
	Aliases: []string{"dignose"},
	Short:   "Explain why a job or instance is in its current state",
	Long: `Explain why a job or instance is in its current state.

Examples:
  weft diagnose wj123
  weft diagnose job wj123
  weft job diagnose wj123
  weft diagnose wi456`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runDiagnose,
}

var diagnoseJobCmd = &cobra.Command{
	Use:   "job <job-id>...",
	Short: jobDiagnoseCmd.Short,
	Long:  jobDiagnoseCmd.Long,
	Args:  usageArgs(cobra.MinimumNArgs(1)),
	RunE:  runJobDiagnose,
}

var diagnoseInstanceCmd = &cobra.Command{
	Use:   "instance <instance-id>",
	Short: instanceDiagnoseCmd.Short,
	Long:  instanceDiagnoseCmd.Long,
	Args:  usageArgs(cobra.ExactArgs(1)),
	RunE:  runInstanceDiagnose,
}

func init() {
	rootCmd.AddCommand(diagnoseCmd)
	diagnoseCmd.AddCommand(diagnoseJobCmd)
	diagnoseCmd.AddCommand(diagnoseInstanceCmd)
}

var runDiagnoseJobFunc = runJobDiagnose
var runDiagnoseInstanceFunc = runInstanceDiagnose

func runDiagnose(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return usageErrorf("requires a job or instance ID")
	}
	first := strings.ToLower(strings.TrimSpace(args[0]))
	switch first {
	case "job", "jobs":
		return runDiagnoseJobFunc(cmd, args[1:])
	case "instance", "instances":
		return runDiagnoseInstanceFunc(cmd, args[1:])
	}
	kind, err := resolveIDTargetKind(args)
	if err == nil {
		if kind == idTargetInstance {
			return runDiagnoseInstanceFunc(cmd, args)
		}
		return runDiagnoseJobFunc(cmd, args)
	}
	if _, sawInstance := idPrefixesSeen(args); sawInstance {
		return err
	}
	// Bare numerics are job IDs throughout job-only commands, and the typo
	// alias `dignose 123` should be forgiving in the same way.
	return runDiagnoseJobFunc(cmd, args)
}
