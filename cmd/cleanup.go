package cmd

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
)

var cleanupCmd = &cobra.Command{
	Use:   "cleanup [host]",
	Short: "Clean up leaked cloud instances or finished host sessions",
	Long: `Clean up leaked terminal cloud instances, or finished sessions and old log
files on a remote host when a host argument is supplied.

Examples:
  weft cleanup --provider runpod --dry-run  # Preview terminal RunPod pods that still exist
  weft cleanup --provider runpod --force    # Delete terminal RunPod pods that still exist
  weft cleanup cool30                    # Clean both
  weft cleanup cool30 --sessions         # Only finished sessions
  weft cleanup cool30 --logs --older-than 3  # Logs > 3 days old
  weft cleanup cool30 --dry-run          # Preview only`,
	Args: usageArgs(cobra.RangeArgs(0, 1)),
	RunE: runCleanup,
}

var (
	cleanupSessions  bool
	cleanupLogs      bool
	cleanupOlderThan int
	cleanupDryRun    bool
	cleanupProvider  string
	cleanupForce     bool
)

func init() {
	rootCmd.AddCommand(cleanupCmd)

	cleanupCmd.Flags().BoolVar(&cleanupSessions, "sessions", false, "Clean finished sessions only")
	cleanupCmd.Flags().BoolVar(&cleanupLogs, "logs", false, "Clean log files only")
	cleanupCmd.Flags().IntVar(&cleanupOlderThan, "older-than", 7, "Only clean items older than N days")
	cleanupCmd.Flags().BoolVar(&cleanupDryRun, "dry-run", false, "Preview without actually deleting")
	cleanupCmd.Flags().StringVar(&cleanupProvider, "provider", "", "Restrict cloud cleanup to a provider (vastai or runpod)")
	cleanupCmd.Flags().BoolVar(&cleanupForce, "force", false, "Delete matching cloud provider instances")
}

func runCleanup(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return runCloudCleanup(cmd)
	}
	if cleanupProvider != "" {
		return fmt.Errorf("--provider is only valid for cloud cleanup")
	}
	if cleanupForce {
		return fmt.Errorf("--force is only valid for cloud cleanup")
	}
	host := args[0]

	// If neither specified, do both
	if !cleanupSessions && !cleanupLogs {
		cleanupSessions = true
		cleanupLogs = true
	}

	if cleanupDryRun {
		fmt.Println("DRY RUN - no changes will be made")
		fmt.Println()
	}

	var totalCleaned int

	if cleanupSessions {
		cleaned, err := cleanupFinishedSessions(host)
		if err != nil {
			return fmt.Errorf("cleanup sessions: %w", err)
		}
		totalCleaned += cleaned
	}

	if cleanupLogs {
		cleaned, err := cleanupOldLogs(host)
		if err != nil {
			return fmt.Errorf("cleanup logs: %w", err)
		}
		totalCleaned += cleaned
	}

	if totalCleaned == 0 {
		fmt.Println("Nothing to clean up")
	}

	return nil
}

type cloudCleanupCandidate struct {
	launch       *db.Launch
	providerInst *cloud.Instance
}

type cloudCleanupListClient interface {
	ListInstancesForCleanup() ([]cloud.Instance, error)
}

func runCloudCleanup(cmd *cobra.Command) error {
	if cleanupSessions || cleanupLogs {
		return fmt.Errorf("--sessions and --logs require a host argument")
	}
	if cleanupDryRun && cleanupForce {
		return fmt.Errorf("--dry-run and --force are mutually exclusive")
	}
	provider, err := normalizeProviderFlag(cleanupProvider)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	launches, err := db.ListLaunches(database)
	if err != nil {
		return fmt.Errorf("list launches: %w", err)
	}

	dryRun := !cleanupForce || cleanupDryRun
	if dryRun {
		fmt.Fprintln(cmd.OutOrStdout(), "DRY RUN - no provider instances will be deleted")
		fmt.Fprintln(cmd.OutOrStdout())
	}

	providerLaunches := cleanupTerminalLaunchesByProvider(launches, provider)
	var candidates []cloudCleanupCandidate
	var checked int
	var alreadyGone int
	var failed []string
	for providerName, launches := range providerLaunches {
		client := cloudClientForDBInstance(providerName)
		if client == nil {
			for _, launch := range launches {
				failed = append(failed, fmt.Sprintf("wi%d: no client for provider %q", launch.ID, launch.Provider))
			}
			continue
		}
		if instances, ok, err := cleanupListProviderInstances(client); err != nil {
			for _, launch := range launches {
				failed = append(failed, fmt.Sprintf("wi%d %s:%s: list %s instances: %v", launch.ID, launch.Provider, launch.ProviderInstanceID, providerName, err))
			}
			continue
		} else if ok {
			providerInstances := cleanupProviderInstancesByID(instances)
			for _, launch := range launches {
				checked++
				inst := providerInstances[launch.ProviderInstanceID]
				if !cleanupProviderInstanceNeedsDelete(inst) {
					alreadyGone++
					continue
				}
				candidates = append(candidates, cloudCleanupCandidate{launch: launch, providerInst: inst})
			}
			continue
		}

		for _, launch := range launches {
			checked++
			inst, err := client.ShowInstance(launch.ProviderInstanceID)
			if errors.Is(err, cloud.ErrInstanceNotFound) {
				alreadyGone++
				continue
			}
			if err != nil {
				failed = append(failed, fmt.Sprintf("wi%d %s:%s: %v", launch.ID, launch.Provider, launch.ProviderInstanceID, err))
				continue
			}
			if !cleanupProviderInstanceNeedsDelete(inst) {
				alreadyGone++
				continue
			}
			candidates = append(candidates, cloudCleanupCandidate{launch: launch, providerInst: inst})
		}
	}

	for _, candidate := range candidates {
		launch := candidate.launch
		inst := candidate.providerInst
		line := fmt.Sprintf("%s wi%d %s:%s db=%s provider=%s label=%q ended=%s",
			cleanupActionVerb(dryRun),
			launch.ID,
			launch.Provider,
			launch.ProviderInstanceID,
			launch.Status,
			inst.Status,
			inst.Label,
			formatCleanupTime(launch.EndedAt),
		)
		fmt.Fprintln(cmd.OutOrStdout(), line)
		if dryRun {
			continue
		}
		client := cloudClientForDBInstance(launch.Provider)
		if err := client.DestroyInstance(launch.ProviderInstanceID); err != nil {
			failed = append(failed, fmt.Sprintf("wi%d %s:%s: delete: %v", launch.ID, launch.Provider, launch.ProviderInstanceID, err))
			continue
		}
		_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
			EventKind: db.EventReconcileSafetyNetDestroy,
			LaunchID:  launch.ID,
			Detail:    fmt.Sprintf("manual cleanup destroyed provider %s, db status %s", launch.ProviderInstanceID, launch.Status),
		})
	}

	if len(candidates) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No terminal provider instances to clean up")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Checked %d terminal launch(es); %d already gone/destroyed; %d candidate(s)\n", checked, alreadyGone, len(candidates))
	if len(failed) > 0 {
		for _, msg := range failed {
			fmt.Fprintf(cmd.ErrOrStderr(), "cleanup warning: %s\n", msg)
		}
		return fmt.Errorf("cleanup completed with %d warning(s)", len(failed))
	}
	return nil
}

func cleanupTerminalLaunchesByProvider(launches []*db.Launch, provider string) map[string][]*db.Launch {
	result := map[string][]*db.Launch{}
	for _, launch := range launches {
		if cleanupTerminalLaunchMatches(launch, provider) {
			result[launch.Provider] = append(result[launch.Provider], launch)
		}
	}
	return result
}

func cleanupListProviderInstances(client cloud.Client) ([]cloud.Instance, bool, error) {
	if cleanupClient, ok := client.(cloudCleanupListClient); ok {
		instances, err := cleanupClient.ListInstancesForCleanup()
		return instances, true, err
	}
	instances, err := client.ListAllInstances()
	if err != nil {
		return nil, true, err
	}
	if instances == nil {
		return nil, false, nil
	}
	return instances, true, nil
}

func cleanupProviderInstancesByID(instances []cloud.Instance) map[string]*cloud.Instance {
	result := make(map[string]*cloud.Instance, len(instances))
	for i := range instances {
		inst := instances[i]
		if strings.TrimSpace(inst.ProviderID) != "" {
			result[inst.ProviderID] = &inst
		}
	}
	return result
}

func cleanupTerminalLaunchMatches(launch *db.Launch, provider string) bool {
	if launch == nil || strings.TrimSpace(launch.ProviderInstanceID) == "" {
		return false
	}
	switch cloud.Provider(launch.Provider) {
	case cloud.ProviderVastai, cloud.ProviderRunpod:
	default:
		return false
	}
	if provider != "" && launch.Provider != provider {
		return false
	}
	switch launch.Status {
	case db.LaunchStatusCompleted, db.LaunchStatusFailed, db.LaunchStatusCancelled:
		return true
	default:
		return false
	}
}

func cleanupProviderInstanceNeedsDelete(inst *cloud.Instance) bool {
	if inst == nil {
		return false
	}
	switch inst.Status {
	case cloud.ProviderStatusDestroyed, cloud.ProviderStatusDead:
		return false
	default:
		return true
	}
}

func cleanupActionVerb(dryRun bool) string {
	if dryRun {
		return "Would delete"
	}
	return "Deleting"
}

func formatCleanupTime(ts *int64) string {
	if ts == nil || *ts == 0 {
		return "unknown"
	}
	return time.Unix(*ts, 0).Format("2006-01-02 15:04:05")
}

func cleanupFinishedSessions(host string) (int, error) {
	fmt.Printf("Checking for finished sessions on %s...\n", host)

	database, err := db.Open()
	if err != nil {
		return 0, fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get list of sessions
	sessions, err := ssh.TmuxListSessions(host)
	if err != nil {
		return 0, err
	}

	if len(sessions) == 0 {
		fmt.Println("No sessions found")
		return 0, nil
	}

	var cleaned int
	for _, sessionName := range sessions {
		// Try to get job info from database
		var job *db.Job
		if strings.HasPrefix(sessionName, "rj-") {
			if jobID, err := strconv.ParseInt(sessionName[3:], 10, 64); err == nil {
				job, err = db.GetJobByID(database, jobID)
				if err != nil {
					return 0, fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
				}
			}
		} else {
			var err error
			job, err = db.GetJob(database, host, sessionName)
			if err != nil {
				return 0, fmt.Errorf("get job %s: %w", sessionName, err)
			}
		}

		// Determine status file path
		var statusFile string
		if job != nil {
			statusFile = session.JobStatusFile(job.ID, job.StartTime, job.SessionName)
		} else if !strings.HasPrefix(sessionName, "rj-") {
			// Legacy session without job record
			statusFile = session.LegacyStatusFile(sessionName)
		}

		var statusContent string
		if statusFile != "" {
			var err error
			statusContent, err = ssh.ReadRemoteFile(host, statusFile)
			if err != nil {
				return 0, fmt.Errorf("read status file %s:%s: %w", host, statusFile, err)
			}
		}

		panePID, err := ssh.GetTmuxPanePID(host, sessionName)
		if err != nil {
			return 0, fmt.Errorf("get tmux pane pid %s: %w", sessionName, err)
		}
		hasChildren, err := ssh.HasChildProcesses(host, panePID)
		if err != nil {
			return 0, fmt.Errorf("check child processes %s: %w", sessionName, err)
		}

		if statusContent != "" || !hasChildren {
			// Session has finished
			if cleanupDryRun {
				fmt.Printf("  Would kill session: %s\n", sessionName)
			} else {
				fmt.Printf("  Killing session: %s\n", sessionName)
				if err := ssh.TmuxKillSession(host, sessionName); err != nil {
					return 0, fmt.Errorf("kill session %s: %w", sessionName, err)
				}
			}
			cleaned++
		}
	}

	if cleaned == 0 {
		fmt.Println("No finished sessions to clean")
	} else {
		fmt.Printf("Cleaned %d session(s)\n", cleaned)
	}

	return cleaned, nil
}

func cleanupOldLogs(host string) (int, error) {
	fmt.Printf("Checking for old log files on %s (older than %d days)...\n", host, cleanupOlderThan)

	var allFiles []string

	// Find legacy archived log files in /tmp (format: /tmp/tmux-*.YYYYMMDD-HHMMSS.log)
	legacyFindCmd := fmt.Sprintf("find /tmp -maxdepth 1 -name 'tmux-*.*.log' -mtime +%d 2>/dev/null", cleanupOlderThan)
	stdout, _, err := ssh.Run(host, legacyFindCmd)
	if err != nil {
		return 0, fmt.Errorf("list legacy logs on %s: %w", host, err)
	}
	for _, file := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if file != "" {
			allFiles = append(allFiles, file)
		}
	}

	// Find new log files in ~/.cache/weft/logs
	// Note: path not quoted to allow tilde expansion
	newFindCmd := fmt.Sprintf("find ~/.cache/weft/logs -maxdepth 1 -type f -mtime +%d 2>/dev/null", cleanupOlderThan)
	stdout, _, err = ssh.Run(host, newFindCmd)
	if err != nil {
		return 0, fmt.Errorf("list logs on %s: %w", host, err)
	}
	for _, file := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if file != "" {
			allFiles = append(allFiles, file)
		}
	}

	var cleaned int
	for _, file := range allFiles {
		filename := path.Base(file)
		if cleanupDryRun {
			fmt.Printf("  Would delete: %s\n", filename)
		} else {
			fmt.Printf("  Deleting: %s\n", filename)
			// Note: path not quoted to allow tilde expansion
			if _, stderr, err := ssh.Run(host, fmt.Sprintf("rm -f %s", file)); err != nil {
				return 0, fmt.Errorf("delete log %s: %s", filename, strings.TrimSpace(stderr))
			}
		}
		cleaned++
	}

	if cleaned == 0 {
		fmt.Println("No old log files to clean")
	} else {
		fmt.Printf("Cleaned %d log file(s)\n", cleaned)
	}

	return cleaned, nil
}
