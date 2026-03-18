package cmd

import (
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
	Use:   "launch <campaign|instance>",
	Short: "Launch campaigns or instances",
}

// --- terminate ---

var terminateCmd = &cobra.Command{
	Use:   "terminate <campaign|instance> <id>",
	Short: "Terminate campaigns or instances",
}

func init() {
	// launch
	launchCampaignCmd := withPluralAlias(verbAlias("campaign", campaignLaunchCmd))
	launchInstanceCmd := withPluralAlias(verbAlias("instance", instanceLaunchCmd))
	rootCmd.AddCommand(launchCmd)
	launchCmd.AddCommand(launchCampaignCmd)
	launchCmd.AddCommand(launchInstanceCmd)
	addCampaignLaunchFlags(launchCampaignCmd)
	addCampaignLaunchFlags(launchInstanceCmd)

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
	} {
		listCmd.AddCommand(sub.cmd)
		if sub.addFlags != nil {
			sub.addFlags(sub.cmd)
		}
	}

	// watch (add noun subcommands to existing watchCmd)
	for _, sub := range []struct {
		cmd      *cobra.Command
		addFlags func(*cobra.Command)
	}{
		{withPluralAlias(verbAlias("campaign [campaign-id]", campaignWatchCmd)), addCampaignWatchFlags},
		{withPluralAlias(verbAlias("instance", instanceWatchCmd)), configureWatchFlags},
		{withPluralAlias(verbAlias("project", projectWatchCmd)), addProjectWatchFlags},
	} {
		watchCmd.AddCommand(sub.cmd)
		if sub.addFlags != nil {
			sub.addFlags(sub.cmd)
		}
	}

	// terminate
	terminateCampaignCmd := withPluralAlias(verbAlias("campaign <campaign-id>", campaignTerminateCmd))
	terminateInstanceCmd := withPluralAlias(verbAlias("instance <id> [id...]", instanceTerminateCmd))
	rootCmd.AddCommand(terminateCmd)
	terminateCmd.AddCommand(terminateCampaignCmd)
	terminateCmd.AddCommand(terminateInstanceCmd)
}
