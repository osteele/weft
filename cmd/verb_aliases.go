package cmd

import (
	"strings"

	"github.com/spf13/cobra"
)

// Verb-noun aliases: "launch campaign" → "campaign launch", etc.
// These provide natural "verb noun" ordering as synonyms for the
// canonical "noun verb" subcommands.

// verbAlias creates a verb-noun alias command that delegates to a canonical
// noun-verb command. It copies behavioral fields from the source so the alias
// behaves identically, including shell completion.
func verbAlias(use string, source *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:               use,
		Short:             source.Short,
		Long:              source.Long,
		Args:              source.Args,
		RunE:              source.RunE,
		ValidArgsFunction: source.ValidArgsFunction,
	}
	return cmd
}

// withPluralAlias adds a plural or singular alias based on the command name.
// If the name ends in "s", adds the singular form; otherwise adds the plural.
func withPluralAlias(cmd *cobra.Command) *cobra.Command {
	name := cmd.Name()
	if name[len(name)-1] == 's' {
		cmd.Aliases = []string{name[:len(name)-1]}
	} else {
		cmd.Aliases = []string{name + "s"}
	}
	return cmd
}

// --- launch ---

var launchCmd = &cobra.Command{
	Use:   "launch <campaign|instance|project>",
	Short: "Launch campaigns, instances, or project jobs",
}

// --- new ---

var newCmd = &cobra.Command{
	Use:   "new <instance>",
	Short: "Create new resources",
}

// --- terminate ---

var terminateCmd = &cobra.Command{
	Use:   "terminate <id...> | terminate <campaign|instance> <id>",
	Short: "Terminate jobs, campaigns, or instances",
	Long: `Terminate by explicit ID prefix:
  - wj... for jobs
  - wi... for cloud instances

You can also use subcommands:
  weft terminate campaign <campaign-id>
  weft terminate instance <id> [id...]`,
	Args: usageArgs(cobra.MinimumNArgs(1)),
	RunE: runTerminate,
}

// --- move ---

var moveCmd = &cobra.Command{
	Use:   "move [job-id]... <destination>",
	Short: "Move queued jobs between hosts/instances or launch new instances",
	Long: `Move queued jobs to a host, instance, or new instance(s).

Use either:
  weft move <job-id>... <destination>
or:
  weft move jobs <job-id>... <destination>`,
	Args: usageArgs(cobra.ArbitraryArgs),
	RunE: runMove,
}

var runMoveDelegate = runJobMove

func runMove(cmd *cobra.Command, args []string) error {
	// Keep bare `weft move` as help, but allow flag-only selector forms like:
	//   weft move --from wi872 --to wi900
	if len(args) == 0 && strings.TrimSpace(jobMoveFrom) == "" && strings.TrimSpace(jobMoveProject) == "" && strings.TrimSpace(jobMoveTo) == "" {
		return cmd.Help()
	}
	return runMoveDelegate(cmd, args)
}

func init() {
	// launch
	launchCampaignCmd := withPluralAlias(verbAlias("campaign", campaignLaunchCmd))
	launchInstanceCmd := withPluralAlias(verbAlias("instance", instanceLaunchCmd))
	launchProjectCmd := withPluralAlias(verbAlias("project", projectLaunchCmd))
	rootCmd.AddCommand(launchCmd)
	launchCmd.AddCommand(launchCampaignCmd)
	launchCmd.AddCommand(launchInstanceCmd)
	launchCmd.AddCommand(launchProjectCmd)
	addCampaignLaunchFlags(launchCampaignCmd)
	addCampaignLaunchFlags(launchInstanceCmd)
	addCampaignLaunchFlags(launchProjectCmd)

	// new
	newInstanceCmd := withPluralAlias(verbAlias("instance [job-id]...", instanceNewCmd))
	rootCmd.AddCommand(newCmd)
	newCmd.AddCommand(newInstanceCmd)
	addInstanceNewFlags(newInstanceCmd)

	// list (add noun subcommands to existing listCmd)
	listJobsCmd := verbAlias("jobs [job-id]...", jobListCmd)
	listCmd.AddCommand(listJobsCmd)
	addListFlags(listJobsCmd)

	for _, sub := range []struct {
		cmd      *cobra.Command
		addFlags func(*cobra.Command)
	}{
		{withPluralAlias(verbAlias("campaigns", campaignListCmd)), addCampaignListFlags},
		{withPluralAlias(verbAlias("instances", instanceListCmd)), nil},
		{withPluralAlias(verbAlias("hosts", hostListCmd)), nil},
		{withPluralAlias(verbAlias("queues", queueListCmd)), nil},
		{withPluralAlias(verbAlias("artifacts", artifactListCmd)), addArtifactListFlags},
		{withPluralAlias(verbAlias("projects", projectListCmd)), addListQueryFlags},
	} {
		listCmd.AddCommand(sub.cmd)
		if sub.addFlags != nil {
			sub.addFlags(sub.cmd)
		}
	}

	// watch (add noun subcommands to existing watchCmd)
	watchSystemCmd := verbAlias("system", instanceWatchCmd)
	for _, sub := range []struct {
		cmd      *cobra.Command
		addFlags func(*cobra.Command)
	}{
		{withPluralAlias(verbAlias("jobs [job-id]...", jobWatchCmd)), addJobWatchFlags},
		{withPluralAlias(verbAlias("campaign [campaign-id]", campaignWatchCmd)), addCampaignWatchFlags},
		{withPluralAlias(verbAlias("instance", instanceWatchCmd)), configureWatchFlags},
		{watchSystemCmd, configureWatchFlags},
		{withPluralAlias(verbAlias("project", projectWatchCmd)), addProjectWatchFlags},
	} {
		watchCmd.AddCommand(sub.cmd)
		if sub.addFlags != nil {
			sub.addFlags(sub.cmd)
		}
	}

	// "start instances" / "start campaign" / "start project" aliases (route to launch)
	startInstanceCmd := withPluralAlias(verbAlias("instance", instanceLaunchCmd))
	startCampaignCmd := withPluralAlias(verbAlias("campaign", campaignLaunchCmd))
	startProjectCmd := withPluralAlias(verbAlias("project", projectLaunchCmd))
	startCmd.AddCommand(startInstanceCmd)
	startCmd.AddCommand(startCampaignCmd)
	startCmd.AddCommand(startProjectCmd)
	addCampaignLaunchFlags(startInstanceCmd)
	addCampaignLaunchFlags(startCampaignCmd)
	addCampaignLaunchFlags(startProjectCmd)

	// "system watch" top-level alias
	systemCmd := &cobra.Command{
		Use:   "system <watch>",
		Short: "System-wide commands",
	}
	systemWatchCmd := verbAlias("watch", instanceWatchCmd)
	configureWatchFlags(systemWatchCmd)
	systemCmd.AddCommand(systemWatchCmd)
	rootCmd.AddCommand(systemCmd)

	// move
	moveJobsCmd := withPluralAlias(verbAlias("jobs <job-id>... <destination>", jobMoveCmd))
	moveCmd.AddCommand(moveJobsCmd)
	rootCmd.AddCommand(moveCmd)
	addJobMoveFlags(moveCmd, &jobMoveEach, &jobMoveProject, &jobMoveTo, &jobMoveFrom)
	addJobMoveFlags(moveJobsCmd, &jobMoveEach, &jobMoveProject, &jobMoveTo, &jobMoveFrom)

	// terminate
	terminateCampaignCmd := withPluralAlias(verbAlias("campaign <campaign-id>", campaignTerminateCmd))
	terminateInstanceCmd := withPluralAlias(verbAlias("instance <id> [id...]", instanceTerminateCmd))
	rootCmd.AddCommand(terminateCmd)
	terminateCmd.AddCommand(terminateCampaignCmd)
	terminateCmd.AddCommand(terminateInstanceCmd)
}
