package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/osteele/weft/internal/secrets"
	"github.com/spf13/cobra"
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage local secrets for job environment variables",
	Long: `Manage local secrets used by job environment variables.

Secrets are stored in the macOS Keychain when available. Other platforms use
a local file with owner-only permissions. Jobs store references such as
HF_TOKEN=secret:hf; weft resolves them only when launching the job.`,
}

var secretSetCmd = &cobra.Command{
	Use:   "set <name> [value]",
	Short: "Store a secret value",
	Args:  usageArgs(cobra.RangeArgs(1, 2)),
	RunE: func(cmd *cobra.Command, args []string) error {
		value := ""
		if len(args) == 2 {
			value = args[1]
		} else {
			stat, err := os.Stdin.Stat()
			if err != nil {
				return fmt.Errorf("stat stdin: %w", err)
			}
			if stat.Mode()&os.ModeCharDevice != 0 {
				return fmt.Errorf("secret value required: pass it as an argument or pipe it on stdin")
			}
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			value = strings.TrimRight(string(data), "\r\n")
		}
		if err := secrets.Set(args[0], value); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Secret %q stored\n", args[0])
		return nil
	},
}

var secretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List stored secret names",
	Args:  usageArgs(cobra.NoArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		names, err := secrets.List()
		if err != nil {
			return err
		}
		for _, name := range names {
			fmt.Fprintln(cmd.OutOrStdout(), name)
		}
		return nil
	},
}

var secretRemoveCmd = &cobra.Command{
	Use:     "remove <name>",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove a stored secret",
	Args:    usageArgs(cobra.ExactArgs(1)),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := secrets.Remove(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Secret %q removed\n", args[0])
		return nil
	},
}

func init() {
	rootCmd.AddCommand(secretCmd)
	secretCmd.AddCommand(secretSetCmd)
	secretCmd.AddCommand(secretListCmd)
	secretCmd.AddCommand(secretRemoveCmd)
}
