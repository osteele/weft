package cmd

import (
	"fmt"

	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var killCmd = &cobra.Command{
	Use:   "kill <host> <session>",
	Short: "Kill a tmux session on a remote host",
	Long: `Kill a specific tmux session on a remote host.

Example:
  remote-jobs kill cool30 train-gpt2`,
	Args: cobra.ExactArgs(2),
	RunE: runKill,
}

func init() {
	rootCmd.AddCommand(killCmd)
}

func runKill(cmd *cobra.Command, args []string) error {
	host := args[0]
	sessionName := args[1]

	fmt.Printf("Killing session '%s' on %s...\n", sessionName, host)

	if err := ssh.TmuxKillSession(host, sessionName); err != nil {
		return fmt.Errorf("kill session: %w", err)
	}

	fmt.Println("Session killed")
	return nil
}
