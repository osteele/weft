package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/spf13/cobra"
)

var (
	buildAgentsTargets                 string
	buildAgentsFromSource              bool
	buildAgentsRecordInstalledIdentity bool
)

var buildAgentsCmd = &cobra.Command{
	Use:    "build-agents",
	Short:  "Prewarm agent binaries in the local cache",
	Hidden: true,
	RunE:   runBuildAgents,
}

func init() {
	buildAgentsCmd.Flags().StringVar(&buildAgentsTargets, "targets", "linux-amd64", "Comma-separated targets (os-arch)")
	buildAgentsCmd.Flags().BoolVar(
		&buildAgentsFromSource,
		"from-source",
		false,
		"Resolve the agent identity from the current Weft source checkout",
	)
	buildAgentsCmd.Flags().BoolVar(
		&buildAgentsRecordInstalledIdentity,
		"record-installed-identity",
		false,
		"Record this executable's prepared agent identity",
	)
	rootCmd.AddCommand(buildAgentsCmd)
}

func runBuildAgents(cmd *cobra.Command, args []string) error {
	if buildAgentsRecordInstalledIdentity && !buildAgentsFromSource {
		return fmt.Errorf("--record-installed-identity requires --from-source")
	}

	sourceVersion := ""
	if buildAgentsFromSource {
		var err error
		sourceVersion, err = agentdeploy.LocalAgentSourceVersion()
		if err != nil {
			return fmt.Errorf("determine source agent version: %w", err)
		}
	}

	builtTargets := make([]string, 0)
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
		version := sourceVersion
		if !buildAgentsFromSource {
			var err error
			version, err = agentdeploy.LocalAgentVersionForTarget(parts[0], parts[1])
			if err != nil {
				return fmt.Errorf("determine agent version for %s: %w", target, err)
			}
		}
		path, err := agentdeploy.EnsureBuiltWithOutput(version, parts[0], parts[1], os.Stderr)
		if err != nil {
			return fmt.Errorf("build agent %s: %w", target, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Built %s -> %s\n", target, path)
		builtTargets = append(builtTargets, target)
	}
	if buildAgentsRecordInstalledIdentity {
		if len(builtTargets) == 0 {
			return fmt.Errorf("--record-installed-identity requires at least one build target")
		}
		if err := agentdeploy.RecordInstalledAgentIdentity(sourceVersion, builtTargets); err != nil {
			return fmt.Errorf("record installed agent identity: %w", err)
		}
	}
	return nil
}
