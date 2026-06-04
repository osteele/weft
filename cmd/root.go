package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ssh"
	"github.com/osteele/weft/internal/util"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var verbose bool

// Version is set at build time via -ldflags
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:   "weft",
	Short: "Manage long-running jobs on remote hosts",
	Long: `Run long-running jobs on remote hosts.

Jobs continue running even when you disconnect, close your laptop,
or lose network connectivity.`,
}

// Execute runs the root command
func Execute() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ssh.Configure(cfg)
	if keys := config.UnknownTOMLKeys; len(keys) > 0 {
		fmt.Fprintf(os.Stderr, "warning: %s: unknown key(s): %s\n", config.ConfigPath(), strings.Join(keys, ", "))
	}
	os.Args = rewriteRootArgs(os.Args, cfg)

	logging.Setup(os.Stderr, "text")
	// Pre-scan os.Args for --verbose, because cobra hasn't parsed flags yet
	// and PersistentPreRun wouldn't run for commands that error before RunE.
	for _, a := range os.Args[1:] {
		if a == "--" {
			break
		}
		if a == "--verbose" {
			verbose = true
			break
		}
		if rest, ok := strings.CutPrefix(a, "--verbose="); ok {
			if v, err := strconv.ParseBool(rest); err == nil {
				verbose = v
			}
			break
		}
	}
	if verbose {
		logging.SetLevel(slog.LevelDebug)
	}

	if os.Getenv("WEFT_PPROF") != "0" {
		startPprof()
	}
	installStackDumpHandler()

	// Initialize operation logger
	initOpLog()
	defer oplog.Close()

	rootCmd.SilenceErrors = true
	rootCmd.SilenceUsage = true

	executedCmd, err := rootCmd.ExecuteC()
	if err == nil {
		return nil
	}

	if executedCmd == nil {
		executedCmd = rootCmd
	}

	// Log database lock errors to oplog for observability
	if db.IsDatabaseLocked(err) {
		cmdName := executedCmd.Name()
		oplog.Log("db_locked", oplog.WithError(err), oplog.WithDetail(cmdName))
	}

	printCommandError(executedCmd, err)
	return err
}

func rewriteRootArgs(args []string, cfg *config.Config) []string {
	if len(args) == 0 {
		return args
	}

	rewritten := append([]string(nil), args...)
	cmdArgs := rewritten[1:]

	// Preserve existing behavior: when no command is provided, insert configured default command.
	if len(cmdArgs) == 0 && cfg != nil && cfg.DefaultCommand != "" && cfg.DefaultCommand != "help" {
		cmdArgs = append(cmdArgs, cfg.DefaultCommand)
	}

	cmdArgs = expandConfiguredAliases(cmdArgs, cfg)
	return append([]string{rewritten[0]}, cmdArgs...)
}

func expandConfiguredAliases(args []string, cfg *config.Config) []string {
	if len(args) == 0 || cfg == nil || len(cfg.Aliases) == 0 {
		return args
	}

	expanded := append([]string(nil), args...)
	seen := map[string]struct{}{}
	const maxAliasExpansions = 16
	for range maxAliasExpansions {
		cmdIndex := firstCommandToken(expanded)
		if cmdIndex < 0 {
			break
		}
		key := expanded[cmdIndex]
		replacement, ok := cfg.Aliases[key]
		if !ok {
			break
		}
		if _, cycle := seen[key]; cycle {
			break
		}
		seen[key] = struct{}{}

		parts := strings.Fields(replacement)
		if len(parts) == 0 {
			break
		}

		next := make([]string, 0, len(expanded)-1+len(parts))
		next = append(next, expanded[:cmdIndex]...)
		next = append(next, parts...)
		next = append(next, expanded[cmdIndex+1:]...)
		expanded = next
	}
	return expanded
}

func firstCommandToken(args []string) int {
	for i, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			return i
		}
	}
	return -1
}

// initOpLog initializes the operation logger based on config.
func initOpLog() {
	cfg, err := config.Load()
	if err != nil || !cfg.IsOperationLogEnabled() {
		return
	}
	oplog.Init(oplog.DefaultLogPath(), cfg.GetOperationLogMaxSize())
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("weft %s\n", Version)
	},
}

func startPprof() {
	ln, err := net.Listen("tcp", "localhost:6060")
	if err != nil {
		// :6060 is usually claimed by another weft process. Fall back to a
		// random local port so this process is still attachable.
		ln, err = net.Listen("tcp", "localhost:0")
		if err != nil {
			slog.Debug("pprof listener failed", "error", err)
			return
		}
	}
	addr := ln.Addr().String()
	slog.Debug("pprof enabled", "addr", "http://"+addr+"/debug/pprof/")
	writePprofPortFile(addr)
	go func() { _ = http.Serve(ln, nil) }()
}

// writePprofPortFile records the pprof listener address so it can be
// discovered without grepping logs. Best-effort; failures ignored.
func writePprofPortFile(addr string) {
	dir := filepath.Join(os.Getenv("HOME"), ".cache", "weft", "pprof")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	sweepStalePprofEntries(dir)
	path := filepath.Join(dir, fmt.Sprintf("%d.addr", os.Getpid()))
	_ = os.WriteFile(path, []byte(addr+"\n"), 0o644)
}

func sweepStalePprofEntries(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		pidStr, ok := strings.CutSuffix(entry.Name(), ".addr")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		if util.IsProcessAlive(pid) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

// installStackDumpHandler lets `kill -USR1 <pid>` capture a wedged process's
// goroutine state without killing it.
func installStackDumpHandler() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			dumpGoroutines()
		}
	}()
}

func dumpGoroutines() {
	dir := filepath.Join(os.Getenv("HOME"), ".cache", "weft", "dumps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("stack dump: mkdir failed", "error", err)
		return
	}
	name := fmt.Sprintf("%d-%s.stack", os.Getpid(), time.Now().UTC().Format("20060102T150405Z"))
	path := filepath.Join(dir, name)
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		slog.Warn("stack dump: write failed", "error", err, "path", path)
		return
	}
	slog.Info("stack dump written", "path", path)
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&verbose, "verbose", false, "Enable debug-level logging")
	rootCmd.AddCommand(versionCmd)
}

func printCommandError(cmd *cobra.Command, err error) {
	stream := cmd.ErrOrStderr()
	fmt.Fprintf(stream, "Error: %v\n", err)
	if db.IsDatabaseLocked(err) {
		fmt.Fprintln(stream, "Hint: database lock contention is usually transient; retry shortly. Cloud instances are reconciled by sync/watch and orphan sweep (`weft sync` can force a pass).")
	}
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
	return strings.HasPrefix(msg, "unknown command") ||
		strings.HasPrefix(msg, "required flag(s)")
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
