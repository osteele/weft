package cmd

import (
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// addJobAddFlagAliases maps job-add flag synonyms to canonical names.
func addJobAddFlagAliases(cmd *cobra.Command) {
	cmd.Flags().SetNormalizeFunc(jobAddFlagAliasNormalizer)
}

func jobAddFlagAliasNormalizer(_ *pflag.FlagSet, name string) pflag.NormalizedName {
	switch name {
	case "dir":
		name = "directory"
	case "project":
		name = "tag"
	}
	return pflag.NormalizedName(name)
}
