package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/logcache"
	"github.com/osteele/remote-jobs/internal/logfiles"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var logCmd = &cobra.Command{
	Use:     "log <job-id>",
	Aliases: []string{"logs"},
	Short:   "View log output from a remote job",
	Long: `View the log file for a specific remote job.

Examples:
  remote-jobs log 25           # View log for job #25 (last 50 lines)
  remote-jobs log 25 -f        # Follow job #25's log
  remote-jobs log 25 -n 100    # Last 100 lines
  remote-jobs log 25 --tail 30 # Last 30 lines (alias for -n)
  remote-jobs log 25 --from 50           # Lines from 50 onwards
  remote-jobs log 25 --from 50 --to 100  # Lines 50-100
  remote-jobs log 25 --to 100            # First 100 lines
  remote-jobs log 25 --grep error        # Lines containing "error"
  remote-jobs log 25 -f --grep epoch     # Follow, filter for "epoch"`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runLog,
}

var (
	logFollow bool
	logLines  int
	logFrom   int
	logTo     int
	logGrep   string
	logFull   bool
)

func init() {
	rootCmd.AddCommand(logCmd)

	logCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log in real-time")
	logCmd.Flags().IntVarP(&logLines, "lines", "n", 50, "Number of lines to show (last N lines)")
	logCmd.Flags().IntVar(&logLines, "tail", 50, "Number of lines to show (alias for --lines)")
	logCmd.Flags().IntVar(&logFrom, "from", 0, "Show lines starting from line N")
	logCmd.Flags().IntVar(&logTo, "to", 0, "Show lines up to line N")
	logCmd.Flags().StringVar(&logGrep, "grep", "", "Filter lines matching pattern")
	logCmd.Flags().BoolVar(&logFull, "full", false, "Show the entire log (alias for --from 1)")
}

func runLog(cmd *cobra.Command, args []string) error {
	jobID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid job ID: %s", args[0])
	}

	if logFull {
		if cmd.Flags().Changed("from") {
			return fmt.Errorf("--full cannot be combined with --from")
		}
		logFrom = 1
	}

	// Validate flag combinations
	hasLineRange := logFrom > 0 || logTo > 0
	if hasLineRange && (cmd.Flags().Changed("lines") || cmd.Flags().Changed("tail")) {
		return fmt.Errorf("--from/--to cannot be used with -n/--lines/--tail")
	}
	if logFollow && logTo > 0 {
		return fmt.Errorf("--follow cannot be used with --to")
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	defaultTailHint := shouldShowDefaultTailHint(cmd)
	tailHintPrinted := false

	// For completed jobs, try local cache first (unless following)
	if !logFollow && job.Status == db.StatusCompleted {
		if cached, err := logcache.Read(jobID); err == nil {
			if defaultTailHint && !tailHintPrinted {
				printDefaultTailHint(jobID)
				tailHintPrinted = true
			}
			output := filterLogContent(cached, logFrom, logTo, logLines, logGrep)
			// Process carriage returns - progress bars use \r to overwrite lines
			fmt.Print(processCarriageReturns(output))
			return nil
		}
		// Fall through to remote fetch if not cached
	}

	// Determine log file path using shared resolver
	logFile, resolved := logfiles.Resolve(job)

	// Check if log file exists (skip when resolver already confirmed it)
	exists := resolved
	if !exists {
		exists, err = ssh.RemoteFileExists(job.Host, logFile)
		if err != nil {
			return fmt.Errorf("check log file: %w", err)
		}
	}
	if !exists {
		return fmt.Errorf("log file not found for job %d on %s", jobID, job.Host)
	}

	// Build the remote command based on flags
	remoteCmd := buildLogCommand(logFile)

	if logFollow {
		// Follow mode - use interactive SSH
		sshCmd := exec.Command("ssh", job.Host, remoteCmd)
		sshCmd.Stdout = os.Stdout
		sshCmd.Stderr = os.Stderr
		return sshCmd.Run()
	}

	// Regular mode
	stdout, stderr, err := ssh.Run(job.Host, remoteCmd)
	if err != nil {
		// Provide user-friendly error messages without leaking internal paths
		if strings.Contains(stderr, "No such file") || strings.Contains(stderr, "cannot open") {
			return fmt.Errorf("log file not found for job %d on %s", jobID, job.Host)
		}
		if strings.Contains(stderr, "Permission denied") {
			return fmt.Errorf("permission denied reading log for job %d on %s", jobID, job.Host)
		}
		if stderr != "" {
			return fmt.Errorf("read log for job %d: %w", jobID, err)
		}
		return fmt.Errorf("read log for job %d: %w", jobID, err)
	}

	// Process carriage returns - progress bars use \r to overwrite lines
	if defaultTailHint && !tailHintPrinted {
		printDefaultTailHint(jobID)
		tailHintPrinted = true
	}
	fmt.Print(processCarriageReturns(stdout))
	return nil
}

// buildLogCommand constructs the remote command for reading log files
// based on the provided flags (--from, --to, -n, --grep, -f)
func buildLogCommand(logFile string) string {
	var cmd string

	// Determine line selection strategy
	// Priority: --from/--to > -n (default)
	if logFrom > 0 && logTo > 0 {
		// Lines from N to M: tail -n +N | head -n (M-N+1)
		count := logTo - logFrom + 1
		if count < 1 {
			count = 1
		}
		cmd = fmt.Sprintf("tail -n +%d %s | head -n %d", logFrom, logFile, count)
	} else if logFrom > 0 {
		// Lines from N onwards
		if logFollow {
			// For follow mode with --from: get from line N then follow
			// Use -F to retry if file doesn't exist yet, suppress errors
			cmd = fmt.Sprintf("tail -n +%d -F %s 2>/dev/null", logFrom, logFile)
		} else {
			cmd = fmt.Sprintf("tail -n +%d %s", logFrom, logFile)
		}
	} else if logTo > 0 {
		// First N lines (up to line N)
		cmd = fmt.Sprintf("head -n %d %s", logTo, logFile)
	} else if logFollow {
		// Follow mode with default or -n lines
		// Use -F to retry if file doesn't exist yet or gets recreated
		// Suppress "cannot open" errors (file might not exist yet for new jobs)
		cmd = fmt.Sprintf("tail -n %d -F %s 2>/dev/null", logLines, logFile)
	} else {
		// Default: last N lines
		cmd = fmt.Sprintf("tail -n %d %s", logLines, logFile)
	}

	// Add grep filter if specified
	if logGrep != "" {
		if logFollow {
			// Use --line-buffered for real-time grep output
			cmd = fmt.Sprintf("%s | grep --line-buffered '%s'", cmd, escapeShellArg(logGrep))
		} else {
			cmd = fmt.Sprintf("%s | grep '%s'", cmd, escapeShellArg(logGrep))
		}
	}

	return cmd
}

func shouldShowDefaultTailHint(cmd *cobra.Command) bool {
	if logFollow || logFull {
		return false
	}
	if logFrom > 0 || logTo > 0 {
		return false
	}
	if cmd.Flags().Changed("lines") || cmd.Flags().Changed("tail") || cmd.Flags().Changed("from") || cmd.Flags().Changed("to") {
		return false
	}
	return true
}

func printDefaultTailHint(jobID int64) {
	fmt.Printf("(showing last %d lines; run 'remote-jobs log %d --full' to see the entire log or adjust -n/--lines)\n\n", logLines, jobID)
}

// escapeShellArg escapes a string for use in single quotes in shell
func escapeShellArg(s string) string {
	// Replace single quotes with '\'' (end quote, escaped quote, start quote)
	result := ""
	for _, c := range s {
		if c == '\'' {
			result += "'\\''"
		} else {
			result += string(c)
		}
	}
	return result
}

// filterLogContent applies line range and grep filters to cached log content
func filterLogContent(content string, from, to, lines int, grepPattern string) string {
	allLines := strings.Split(content, "\n")

	// Remove trailing empty line if content ends with newline
	if len(allLines) > 0 && allLines[len(allLines)-1] == "" {
		allLines = allLines[:len(allLines)-1]
	}

	var result []string

	// Apply line range filters
	if from > 0 && to > 0 {
		// Lines from N to M (1-indexed)
		start := from - 1
		end := to
		if start < 0 {
			start = 0
		}
		if end > len(allLines) {
			end = len(allLines)
		}
		if start < len(allLines) {
			result = allLines[start:end]
		}
	} else if from > 0 {
		// Lines from N onwards (1-indexed)
		start := from - 1
		if start < 0 {
			start = 0
		}
		if start < len(allLines) {
			result = allLines[start:]
		}
	} else if to > 0 {
		// First N lines
		end := to
		if end > len(allLines) {
			end = len(allLines)
		}
		result = allLines[:end]
	} else {
		// Default: last N lines
		start := len(allLines) - lines
		if start < 0 {
			start = 0
		}
		result = allLines[start:]
	}

	// Apply grep filter if specified
	if grepPattern != "" {
		re, err := regexp.Compile(grepPattern)
		if err != nil {
			// Fall back to simple substring match
			var filtered []string
			for _, line := range result {
				if strings.Contains(line, grepPattern) {
					filtered = append(filtered, line)
				}
			}
			result = filtered
		} else {
			var filtered []string
			for _, line := range result {
				if re.MatchString(line) {
					filtered = append(filtered, line)
				}
			}
			result = filtered
		}
	}

	if len(result) == 0 {
		return ""
	}
	return strings.Join(result, "\n") + "\n"
}

// processCarriageReturns handles \r characters used by progress bars.
// For each line, it returns only the final segment after the last \r,
// simulating what the terminal would display.
func processCarriageReturns(content string) string {
	lines := strings.Split(content, "\n")
	var result []string

	for _, line := range lines {
		// If line contains \r, take only the part after the last \r
		if idx := strings.LastIndex(line, "\r"); idx >= 0 {
			line = line[idx+1:]
		}
		result = append(result, line)
	}

	return strings.Join(result, "\n")
}
