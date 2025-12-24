package cmd

import (
	"fmt"
	"os"

	"github.com/osteele/remote-jobs/internal/config"
	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:   "remote-jobs",
	Short: "Manage persistent tmux jobs on remote hosts",
	Long: `Remote Jobs manages persistent tmux sessions on remote hosts.

Jobs continue running even when you disconnect, close your laptop,
or lose network connectivity. Use SSH + tmux to create robust,
long-running processes on remote machines.`,
}

// Execute runs the root command
func Execute() error {
	// If no args provided, check config for default command
	if len(os.Args) == 1 {
		cfg, _ := config.Load()
		if cfg != nil && cfg.DefaultCommand != "" && cfg.DefaultCommand != "help" {
			// Insert the default command as the first argument
			os.Args = append(os.Args, cfg.DefaultCommand)
		}
	}
	return rootCmd.Execute()
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("remote-jobs %s\n", Version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
