package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/osteele/remote-jobs/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Version is set at build time via -ldflags
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:   "remote-jobs",
	Short: "Manage long-running jobs on remote hosts",
	Long: `Run long-running jobs on remote hosts.

Jobs continue running even when you disconnect, close your laptop,
or lose network connectivity.`,
}

// Execute runs the root command
func Execute() error {
	// If no args provided, check config for default command
	if len(os.Args) == 1 {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if cfg != nil && cfg.DefaultCommand != "" && cfg.DefaultCommand != "help" {
			// Insert the default command as the first argument
			os.Args = append(os.Args, cfg.DefaultCommand)
		}
	}

	rootCmd.SilenceErrors = true
	rootCmd.SilenceUsage = true

	executedCmd, err := rootCmd.ExecuteC()
	if err == nil {
		return nil
	}

	if executedCmd == nil {
		executedCmd = rootCmd
	}
	printCommandError(executedCmd, err)
	return err
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

func printCommandError(cmd *cobra.Command, err error) {
	stream := cmd.ErrOrStderr()
	fmt.Fprintf(stream, "Error: %v\n", err)
	if isUsageError(err) {
		fmt.Fprintln(stream)
		fmt.Fprint(stream, cmd.UsageString())
	}
}

func isUsageError(err error) bool {
	if err == nil {
		return false
	}
	var ue usageError
	if errors.As(err, &ue) {
		return true
	}
	var notExist *pflag.NotExistError
	if errors.As(err, &notExist) {
		return true
	}
	var valueRequired *pflag.ValueRequiredError
	if errors.As(err, &valueRequired) {
		return true
	}
	var invalidValue *pflag.InvalidValueError
	if errors.As(err, &invalidValue) {
		return true
	}
	var invalidSyntax *pflag.InvalidSyntaxError
	if errors.As(err, &invalidSyntax) {
		return true
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command")
}

type usageError struct {
	err error
}

func (e usageError) Error() string {
	return e.err.Error()
}

func (e usageError) Unwrap() error {
	return e.err
}

func usageArgs(fn cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if fn == nil {
			return nil
		}
		if err := fn(cmd, args); err != nil {
			return usageError{err: err}
		}
		return nil
	}
}

func usageErrorf(format string, args ...interface{}) error {
	return usageError{err: fmt.Errorf(format, args...)}
}
