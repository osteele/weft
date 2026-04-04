package cmd

import (
	"fmt"
	"sort"
	"strings"

	"github.com/osteele/weft/internal/config"
	"github.com/spf13/cobra"
)

var aliasesCmd = &cobra.Command{
	Use:   "aliases",
	Short: "List configured command aliases",
	Args:  cobra.NoArgs,
	RunE:  runAliases,
}

var listAliasesCmd = &cobra.Command{
	Use:   "aliases",
	Short: "List configured command aliases",
	Args:  cobra.NoArgs,
	RunE:  runAliases,
}

func init() {
	rootCmd.AddCommand(aliasesCmd)
	listCmd.AddCommand(listAliasesCmd)
}

func runAliases(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	keys := make([]string, 0, len(cfg.Aliases))
	for key := range cfg.Aliases {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	if len(keys) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No aliases configured in %s\n", config.ConfigPath())
		return nil
	}

	for _, key := range keys {
		value := strings.TrimSpace(cfg.Aliases[key])
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", key, value)
	}
	return nil
}
