package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/spf13/cobra"
)

var buildAgentsTargets string

var buildAgentsCmd = &cobra.Command{
	Use:    "build-agents",
	Short:  "Prewarm agent binaries in the local cache",
	Hidden: true,
	RunE:   runBuildAgents,
}

func init() {
	buildAgentsCmd.Flags().StringVar(&buildAgentsTargets, "targets", "linux-amd64", "Comma-separated targets (os-arch)")
	rootCmd.AddCommand(buildAgentsCmd)
}

func runBuildAgents(cmd *cobra.Command, args []string) error {
	version, err := agentdeploy.LocalAgentVersion()
	if err != nil {
		return fmt.Errorf("determine agent version: %w", err)
	}

	targets := strings.Split(buildAgentsTargets, ",")
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		parts := strings.SplitN(target, "-", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid target %q (expected os-arch)", target)
		}
		path, err := agentdeploy.EnsureBuilt(version, parts[0], parts[1])
		if err != nil {
			return fmt.Errorf("build agent %s: %w", target, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Built %s -> %s\n", target, path)
	}
	return nil
}
