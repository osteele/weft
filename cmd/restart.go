package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var restartCmd = &cobra.Command{
	Use:   "restart <host> <session>",
	Short: "Restart a job using saved metadata",
	Long: `Restart a job using its saved metadata file.

This kills the existing session (if any) and starts a new one
with the same command and working directory.

Example:
  remote-jobs restart cool30 train-gpt2`,
	Args: cobra.ExactArgs(2),
	RunE: runRestart,
}

func init() {
	rootCmd.AddCommand(restartCmd)
}

func runRestart(cmd *cobra.Command, args []string) error {
	host := args[0]
	sessionName := args[1]

	// Read metadata from remote
	metadataFile := session.MetadataFile(sessionName)
	content, err := ssh.ReadRemoteFile(host, metadataFile)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	if content == "" {
		return fmt.Errorf("metadata file not found: %s", metadataFile)
	}

	metadata := session.ParseMetadata(content)
	workingDir := metadata["working_dir"]
	command := metadata["command"]
	description := metadata["description"]

	if workingDir == "" || command == "" {
		return fmt.Errorf("invalid metadata file (missing working_dir or command)")
	}

	fmt.Printf("Restarting job '%s' on %s\n", sessionName, host)
	fmt.Printf("Working directory: %s\n", workingDir)
	fmt.Printf("Command: %s\n", command)
	if description != "" {
		fmt.Printf("Description: %s\n", description)
	}

	// Kill existing session if running
	exists, _ := ssh.TmuxSessionExists(host, sessionName)
	if exists {
		fmt.Printf("Killing existing session...\n")
		if err := ssh.TmuxKillSession(host, sessionName); err != nil {
			return fmt.Errorf("kill session: %w", err)
		}
	}

	// Create file paths
	logFile := session.LogFile(sessionName)
	statusFile := session.StatusFile(sessionName)

	// Archive any existing log file
	archiveCmd := fmt.Sprintf("if [ -f '%s' ]; then mv '%s' '%s.$(date +%%Y%%m%%d-%%H%%M%%S).log'; fi",
		logFile, logFile, strings.TrimSuffix(logFile, ".log"))
	ssh.RunWithRetry(host, archiveCmd)

	// Remove old status file
	ssh.RunWithRetry(host, fmt.Sprintf("rm -f '%s'", statusFile))

	// Update metadata with new start time
	startTime := time.Now().Unix()
	newMetadata := session.FormatMetadata(workingDir, command, host, description, startTime)
	metadataCmd := fmt.Sprintf("cat > '%s' << 'METADATA_EOF'\n%s\nMETADATA_EOF", metadataFile, newMetadata)
	ssh.RunWithRetry(host, metadataCmd)

	// Create the wrapped command (PIPESTATUS[0] gets the command's exit, not tee's)
	// Note: $ must be escaped as \$ since command runs inside double quotes via ssh
	// User command is wrapped in () to ensure all output goes through tee
	wrappedCommand := fmt.Sprintf(
		"cd '%s' && (%s) 2>&1 | tee '%s'; EXIT_CODE=\\${PIPESTATUS[0]}; echo \\$EXIT_CODE > '%s'",
		workingDir, command, logFile, statusFile)

	// Start tmux session
	tmuxCmd := fmt.Sprintf("tmux new-session -d -s '%s' bash -c \"%s\"", sessionName, wrappedCommand)
	if _, stderr, err := ssh.Run(host, tmuxCmd); err != nil {
		return fmt.Errorf("start session: %s", stderr)
	}

	fmt.Println("✓ Session restarted successfully")

	// Record in database
	database, err := db.Open()
	if err == nil {
		defer database.Close()
		jobID, err := db.RecordStart(database, host, sessionName, workingDir, command, startTime, description)
		if err == nil {
			fmt.Printf("Job ID: %d\n", jobID)
		}
	}

	return nil
}

// Helper for parsing integer from metadata
func parseMetadataInt(metadata map[string]string, key string) int64 {
	if val, ok := metadata[key]; ok {
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			return i
		}
	}
	return 0
}
