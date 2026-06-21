package cmd

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	xterm "github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/retrypolicy"
	"github.com/osteele/weft/internal/sshaudit"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/osteele/weft/internal/util"
	"github.com/spf13/cobra"
)

var instanceCmd = &cobra.Command{
	Use:     "instance",
	Aliases: []string{"instances"},
	Short:   "Manage execution targets and cloud GPU instances",
}

var instanceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List execution targets",
	RunE:  runInstanceList,
}

var instanceStatusCmd = &cobra.Command{
	Use:   "status <id> [id...]",
	Short: "Show execution target or cloud instance status",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runInstanceStatus,
}

var instanceInfoCmd = &cobra.Command{
	Use:   "info <id> [id...]",
	Short: "Show execution target or cloud instance status",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runInstanceStatus,
}

var instanceTerminateCmd = &cobra.Command{
	Use:     "terminate <id> [id...]",
	Aliases: []string{"cancel"},
	Short:   "Terminate cloud instances and destroy their cloud instances",
	Args:    cobra.MinimumNArgs(1),
	RunE:    runInstanceTerminate,
}

var instanceSSHCmd = &cobra.Command{
	Use:   "ssh <id>",
	Short: "SSH into a cloud instance",
	Long: `Waits for the cloud instance to be ready, then connects
via SSH. Use --print to print the SSH command instead of connecting.`,
	Args: cobra.ExactArgs(1),
	RunE: runInstanceSSH,
}

var instanceSSHPrint bool
var instanceSubmitCommand string
var instanceStatusSync bool
var instanceStatusNoSync bool

var instanceSubmitCmd = &cobra.Command{
	Use:   "submit <instance-id> <job-id>",
	Short: "Resubmit a job to a cloud instance in grace period",
	Long: `Re-syncs sources and resubmits a job to a cloud instance that is in
grace period (waiting after job failure). Use --command to override the job command.`,
	Args: cobra.ExactArgs(2),
	RunE: runInstanceSubmit,
}

var instanceExtendCmd = &cobra.Command{
	Use:   "extend <instance-id> [duration]",
	Short: "Extend the grace period of a cloud instance",
	Long:  `Extends the grace period deadline. Default extension is 15m.`,
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runInstanceExtend,
}

var instanceReleaseCmd = &cobra.Command{
	Use:   "release <instance-id>",
	Short: "Release a cloud instance from grace period (clean self-destruct)",
	Long: `Signals the instance to write completion markers and self-destruct.
Unlike terminate, this does a clean shutdown with proper completion markers.`,
	Args: cobra.ExactArgs(1),
	RunE: runInstanceRelease,
}

var instanceCordonReason string

var instanceCordonCmd = &cobra.Command{
	Use:   "cordon <instance-id|host> [id|host...]",
	Short: "Mark an execution target so autopilot stops routing new jobs to it",
	Long: `Cordon flags an execution target as ineligible for new placements. The
target keeps running and any active job continues to completion, but autopilot
and explicit reuse paths will skip it when placing new jobs. Use 'uncordon' to
clear the flag.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runInstanceCordon,
}

var instanceUncordonCmd = &cobra.Command{
	Use:   "uncordon <instance-id|host> [id|host...]",
	Short: "Clear the cordon flag on an execution target",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runInstanceUncordon,
}

var instanceWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch cloud instances, on-prem jobs, and unplaced jobs",
	Long: `Watch the full active system state.

In an interactive terminal this launches an instance-centric TUI. In plain mode
it prints periodic summaries of cloud instances, on-prem active jobs, and
unplaced jobs.`,
	RunE: runWatchCommand,
}

var instanceLaunchCmd = &cobra.Command{
	Use:     "launch",
	Aliases: []string{"run", "start"},
	Short:   "Interactively select and launch cloud instances for unplaceable jobs",
	Long:    instanceLaunchLong,
	RunE:    runInstanceLaunch,
}

var (
	instanceNewJobs        string
	instanceNewProject     string
	instanceNewStrategy    string
	instanceNewMinSurvival float64
	instanceNewDryRun      bool
	instanceNewYes         bool
	instanceNewWait        bool
	instanceNewTimeout     time.Duration
)

var instanceNewCmd = &cobra.Command{
	Use:   "new [job-id]...",
	Short: "Launch one new instance and rebalance queued jobs onto it",
	Long: `Launch one new cloud instance for queued work, then start that instance
with the selected launch group.

The command chooses an anchor queued job, adds compatible queued jobs from the
same scope, opens move intents so autopilot stays hands-off, and launches the
new instance with the full initial job list. Source claims are transferred by
the launch path and the command waits for agent_ready before confirming the
move intents.

Without --yes or --dry-run, the command previews the selected launch group and
asks for confirmation before it creates move intents or launches an instance.

Examples:
  weft instance new
  weft instance new --yes
  weft instance new --project myproj --yes
  weft instance new wj42 wj43 --dry-run`,
	Args: usageArgs(cobra.ArbitraryArgs),
	RunE: runInstanceNew,
}

func init() {
	rootCmd.AddCommand(instanceCmd)
	instanceCmd.AddCommand(instanceListCmd)
	instanceCmd.AddCommand(instanceStatusCmd)
	instanceCmd.AddCommand(instanceInfoCmd)
	instanceCmd.AddCommand(instanceTerminateCmd)
	instanceCmd.AddCommand(instanceSSHCmd)
	instanceCmd.AddCommand(instanceSubmitCmd)
	instanceCmd.AddCommand(instanceExtendCmd)
	instanceCmd.AddCommand(instanceReleaseCmd)
	instanceCmd.AddCommand(instanceCordonCmd)
	instanceCmd.AddCommand(instanceUncordonCmd)
	instanceCmd.AddCommand(instanceWatchCmd)
	instanceCmd.AddCommand(instanceLaunchCmd)
	instanceCmd.AddCommand(instanceNewCmd)
	instanceCmd.AddCommand(instanceDiagnoseCmd)
	instanceCmd.AddCommand(instanceDiskReportCmd)
	instanceCmd.AddCommand(instanceMarkCreditExhaustedCmd)

	instanceSSHCmd.Flags().BoolVar(&instanceSSHPrint, "print", false, "Print the SSH command instead of connecting")
	instanceSubmitCmd.Flags().StringVar(&instanceSubmitCommand, "command", "", "Override the job command")
	instanceCordonCmd.Flags().StringVar(&instanceCordonReason, "reason", "", "Optional human-readable reason (recorded with the cordon)")
	for _, cmd := range []*cobra.Command{instanceStatusCmd, instanceInfoCmd} {
		cmd.Flags().BoolVar(&instanceStatusSync, "sync", false, "Perform live provider/R2 refresh before showing status")
		cmd.Flags().BoolVar(&instanceStatusNoSync, "no-sync", false, "Skip live provider/R2 refresh")
	}

	configureWatchFlags(instanceWatchCmd)
	addInstanceLaunchFlags(instanceLaunchCmd)
	addInstanceNewFlags(instanceNewCmd)
}

func addInstanceNewFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&instanceNewJobs, "jobs", "", "Comma-separated job IDs/ranges to consider")
	cmd.Flags().StringVar(&instanceNewProject, "project", "", "Restrict queued jobs to a project")
	cmd.Flags().StringVar(&instanceNewStrategy, "strategy", "fastest", "Offer selection strategy: cheap, fast, or fastest")
	cmd.Flags().Float64Var(&instanceNewMinSurvival, "min-survival", 0.4, "Minimum survival probability (0-1); offers below this are skipped")
	cmd.Flags().BoolVar(&instanceNewDryRun, "dry-run", false, "Print the selected launch group without launching")
	cmd.Flags().BoolVarP(&instanceNewYes, "yes", "y", false, "Run without interactive confirmation")
	cmd.Flags().BoolVar(&instanceNewWait, "wait", true, "Wait for agent_ready before confirming move intents")
	cmd.Flags().DurationVar(&instanceNewTimeout, "timeout", 20*time.Minute, "Maximum time to wait for agent_ready")
}

func runInstanceNew(cmd *cobra.Command, args []string) error {
	jobScope, err := parseInstanceNewJobScope(args, instanceNewJobs)
	if err != nil {
		return err
	}
	strategy, err := parseInstanceNewStrategy(instanceNewStrategy)
	if err != nil {
		return err
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	statusLine := newInstanceStatusLine(cmd.ErrOrStderr())
	defer statusLine.Finish()

	maxAttempts := instanceNewMaxAttempts()
	var res orchestration.NewInstanceResult
	for attempt := 0; attempt < maxAttempts; attempt++ {
		res, err = orchestration.LaunchNewInstanceWithRebalance(cmd.Context(), database, cfg, orchestration.NewInstanceOptions{
			JobScope:     jobScope,
			Project:      instanceNewProject,
			Strategy:     strategy,
			MinSurvival:  instanceNewMinSurvival,
			DryRun:       instanceNewDryRun,
			WaitReady:    instanceNewWait,
			ReadyTimeout: instanceNewTimeout,
			OnStatus: func(message string) {
				statusLine.Update(message)
			},
			OnEvent: func(event campaign.LaunchEvent) {
				if line := formatInstanceNewLaunchEvent(event); line != "" {
					statusLine.Println(line)
				}
			},
			OnCampaign: func(campaignID int64) {
				statusLine.Println(fmt.Sprintf("Launching one instance in batch %d...", campaignID))
			},
			ConfirmBeforeLaunch: func(res orchestration.NewInstanceResult) (bool, error) {
				if instanceNewDryRun || instanceNewYes {
					return true, nil
				}
				return confirmInstanceNewLaunch(cmd.InOrStdin(), cmd.OutOrStdout(), res)
			},
		})
		if err == nil {
			statusLine.Finish()
			printInstanceNewResult(cmd.OutOrStdout(), res)
			return nil
		}
		if attempt >= maxAttempts-1 || !orchestration.IsRetryableNewInstanceLaunchError(err) {
			break
		}
		delay, ok := retrypolicy.BackoffDelay(attempt)
		if !ok {
			break
		}
		statusLine.Println(fmt.Sprintf("New instance attempt %d/%d failed: %v", attempt+1, maxAttempts, err))
		statusLine.Update(fmt.Sprintf("Retrying new instance launch in %s (next attempt %d/%d)", delay, attempt+2, maxAttempts))
		if waitErr := waitForNewInstanceRetry(cmd.Context(), delay); waitErr != nil {
			statusLine.Finish()
			return waitErr
		}
	}
	if err != nil {
		statusLine.Finish()
		if maxAttempts > 1 && orchestration.IsRetryableNewInstanceLaunchError(err) {
			return fmt.Errorf("new instance launch failed after %d attempts: %w", maxAttempts, err)
		}
		return err
	}
	statusLine.Finish()
	printInstanceNewResult(cmd.OutOrStdout(), res)
	return nil
}

func instanceNewMaxAttempts() int {
	if instanceNewYes && !instanceNewDryRun {
		return retrypolicy.MaxAttempts()
	}
	return 1
}

func waitForNewInstanceRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type instanceStatusLine struct {
	mu          sync.Mutex
	w           io.Writer
	interactive bool
	frames      []string
	frame       int
	message     string
	rendered    bool
	done        chan struct{}
	closed      bool
	lastPlain   time.Time
}

func newInstanceStatusLine(w io.Writer) *instanceStatusLine {
	s := &instanceStatusLine{
		w:      w,
		frames: []string{"|", "/", "-", "\\"},
	}
	if f, ok := w.(*os.File); ok && xterm.IsTerminal(f.Fd()) {
		s.interactive = true
		s.done = make(chan struct{})
		go s.renderLoop()
	}
	return s
}

func (s *instanceStatusLine) Update(message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.message = message
	if s.interactive {
		s.renderLocked()
		return
	}
	now := time.Now()
	if s.lastPlain.IsZero() || now.Sub(s.lastPlain) >= time.Minute {
		fmt.Fprintln(s.w, message)
		s.lastPlain = now
	}
}

func (s *instanceStatusLine) Println(message string) {
	message = strings.TrimRight(message, "\r\n")
	if message == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.interactive {
		s.clearLocked()
	}
	s.message = ""
	fmt.Fprintln(s.w, message)
}

func (s *instanceStatusLine) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.done != nil {
		close(s.done)
	}
	if s.interactive {
		s.clearLocked()
	}
}

func (s *instanceStatusLine) renderLoop() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			if s.message != "" {
				s.renderLocked()
			}
			s.mu.Unlock()
		}
	}
}

func (s *instanceStatusLine) renderLocked() {
	if len(s.frames) == 0 {
		fmt.Fprintf(s.w, "\r\033[K%s", s.message)
		s.rendered = true
		return
	}
	frame := s.frames[s.frame%len(s.frames)]
	s.frame++
	fmt.Fprintf(s.w, "\r\033[K%s %s", frame, s.message)
	s.rendered = true
}

func (s *instanceStatusLine) clearLocked() {
	if !s.rendered {
		return
	}
	fmt.Fprint(s.w, "\r\033[K")
	s.rendered = false
}

func parseInstanceNewJobScope(args []string, jobsFlag string) (map[int64]struct{}, error) {
	if len(args) > 0 && strings.TrimSpace(jobsFlag) != "" {
		return nil, usageErrorf("pass job IDs positionally or via --jobs, not both")
	}
	var raw []string
	if strings.TrimSpace(jobsFlag) != "" {
		raw = []string{jobsFlag}
	} else {
		raw = args
	}
	if len(raw) == 0 {
		return nil, nil
	}
	jobIDs, err := ParseJobIDs(raw)
	if err != nil {
		return nil, err
	}
	scope := make(map[int64]struct{}, len(jobIDs))
	for _, jobID := range jobIDs {
		scope[jobID] = struct{}{}
	}
	return scope, nil
}

func parseInstanceNewStrategy(raw string) (bidding.SelectionStrategy, error) {
	switch strategy := bidding.SelectionStrategy(strings.ToLower(strings.TrimSpace(raw))); strategy {
	case "", bidding.StrategyFastest:
		return bidding.StrategyFastest, nil
	case bidding.StrategyFast:
		return bidding.StrategyFast, nil
	case bidding.StrategyCheap:
		return bidding.StrategyCheap, nil
	default:
		return "", usageErrorf("invalid --strategy %q (want cheap, fast, or fastest)", raw)
	}
}

func formatInstanceNewLaunchEvent(event campaign.LaunchEvent) string {
	switch event.Kind {
	case campaign.LaunchEventCampaignStatus:
		if strings.TrimSpace(event.Phase) != "" {
			return "  " + strings.TrimSpace(event.Phase)
		}
	case campaign.LaunchEventGroupAssets:
		return fmt.Sprintf("  %s: staging (%d/%d assets ready)", event.Group.GPUSpec(), event.AssetsReady, event.AssetsTotal)
	case campaign.LaunchEventGroupRetry:
		return fmt.Sprintf("  %s: retrying with replacement offer (attempt %d/%d)", event.Group.GPUSpec(), event.RetryAttempt, event.RetryMax)
	case campaign.LaunchEventGroupPhase:
		if strings.TrimSpace(event.Phase) != "" {
			return fmt.Sprintf("  %s: %s", event.Group.GPUSpec(), strings.TrimSpace(event.Phase))
		}
	}
	return ""
}

func printInstanceNewResult(out io.Writer, res orchestration.NewInstanceResult) {
	if res.DryRun {
		printInstanceNewPreview(out, res, "Would launch")
		return
	}
	if res.Canceled {
		fmt.Fprintln(out, "Canceled.")
		return
	}
	for _, instanceID := range res.InstanceIDs {
		fmt.Fprintf(out, "Launched instance %s\n", ids.FormatInstanceID(instanceID))
	}
	fmt.Fprintf(out, "Initial jobs: %s\n", ids.FormatJobIDListCompact(res.JobIDs))
	if res.Warning != "" {
		fmt.Fprintf(out, "Warning: %s\n", res.Warning)
	}
}

func confirmInstanceNewLaunch(in io.Reader, out io.Writer, res orchestration.NewInstanceResult) (bool, error) {
	printInstanceNewPreview(out, res, "Selected")
	fmt.Fprint(out, "Launch this instance? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func printInstanceNewPreview(out io.Writer, res orchestration.NewInstanceResult, verb string) {
	fmt.Fprintf(out, "%s one instance for anchor %s with %d job(s): %s\n",
		verb, ids.FormatJobID(res.AnchorJobID), len(res.JobIDs), ids.FormatJobIDListCompact(res.JobIDs))
	if res.Offer != nil {
		fmt.Fprintf(out, "Offer: %s %s $%.2f/hr\n", res.Offer.Provider, res.Offer.GPUName, res.Offer.CostPerHour)
	}
}

// newR2ClientFromConfig loads config and creates an R2 client.
// Returns an error if R2 is not configured.
func newR2ClientFromConfig() (*r2.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	client, err := buildR2Client(cfg)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("R2 not configured in ~/.config/weft/config.toml")
	}
	return client, nil
}

// getGraceInstance fetches a cloud instance and verifies it is in grace period.
func getGraceInstance(database *sql.DB, instanceID int64) (*db.Launch, error) {
	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		return nil, fmt.Errorf("get instance: %w", err)
	}
	if ci == nil {
		return nil, fmt.Errorf("instance %s not found", ids.FormatInstanceID(instanceID))
	}
	if ci.Status != db.LaunchStatusGrace {
		return nil, fmt.Errorf("instance %s is not in grace period (status: %s)", ids.FormatInstanceID(instanceID), ci.Status)
	}
	return ci, nil
}

func runInstanceList(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	targets, err := db.ListExecutionTargets(database)
	if err != nil {
		return fmt.Errorf("list execution targets: %w", err)
	}
	if len(targets) == 0 {
		fmt.Println("No execution targets.")
		return nil
	}

	launches, _ := db.ListLaunches(database)
	launchByID := make(map[int64]*db.Launch, len(launches))
	for _, launch := range launches {
		if launch != nil {
			launchByID[launch.ID] = launch
		}
	}
	jobCounts, _ := db.GetLaunchJobCounts(database)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "TARGET\tKIND\tSTATUS\tGPU\tJOBS\tCURRENT\tBACKING\tDEADLINE\tUPDATED\n")

	now := time.Now()
	for _, target := range targets {
		statusStr := string(target.Status)
		if target.Status == db.ExecutionTargetRunning && target.LaunchID != nil {
			if launch := launchByID[*target.LaunchID]; launch != nil && !launch.IsAgentReady() {
				if launch.BootstrapDeadlineExceeded(now) {
					statusStr = "no-agent"
				} else {
					statusStr = "bootstrapping"
				}
			}
		}
		if target.Cordoned {
			statusStr += " [cordoned]"
		}
		gpuSpec := executionTargetGPU(target)
		current := "—"
		if target.CurrentJobID != nil {
			current = ids.FormatJobID(*target.CurrentJobID)
		}
		backing := target.Host
		jobs := target.QueueDepth
		if target.LaunchID != nil {
			launch := launchByID[*target.LaunchID]
			backing = ids.FormatInstanceID(*target.LaunchID)
			if launch != nil {
				if providerID := launch.EffectiveProviderID(); providerID != "" {
					backing += "/" + providerID
				}
			}
			jobs = jobCounts[*target.LaunchID]
		}
		deadline := "—"
		if target.DeadlineUnix != nil && *target.DeadlineUnix > 0 {
			deadline = time.Unix(*target.DeadlineUnix, 0).Format("01/02 15:04")
		}
		updated := time.Unix(target.UpdatedAt, 0).Format("01/02 15:04")

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			executionTargetDisplayID(target), target.Kind, statusStr, gpuSpec, jobs, current, backing, deadline, updated)
	}
	w.Flush()
	return nil
}

func executionTargetDisplayID(target *db.ExecutionTarget) string {
	if target == nil {
		return "—"
	}
	if target.LaunchID != nil {
		return ids.FormatInstanceID(*target.LaunchID)
	}
	return target.Host
}

func executionTargetGPU(target *db.ExecutionTarget) string {
	if target == nil {
		return "—"
	}
	if target.GPUClass == "" && target.GPUMemGB == 0 && target.NumGPUs == 0 {
		return "—"
	}
	label := target.GPUClass
	if label == "" {
		label = "GPU"
	}
	if target.GPUMemGB > 0 {
		label += fmt.Sprintf(" %dGB", target.GPUMemGB)
	}
	if target.NumGPUs > 1 {
		label = fmt.Sprintf("%dx %s", target.NumGPUs, label)
	}
	return label
}

func runInstanceStatus(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	forceLive, noLive := instanceStatusSyncMode(cmd)
	for i, arg := range args {
		if i > 0 {
			fmt.Println()
		}

		id, err := ids.ParseInstanceID(arg)
		if err != nil {
			if handled, hostErr := printInventoryTargetStatus(database, arg); handled || hostErr != nil {
				return hostErr
			}
			return fmt.Errorf("invalid instance ID or host %q: %w", arg, err)
		}

		ci, err := db.GetLaunch(database, id)
		if err != nil {
			return fmt.Errorf("get instance %s: %w", ids.FormatInstanceID(id), err)
		}
		if ci == nil {
			fmt.Fprintf(os.Stderr, "Instance %s not found\n", ids.FormatInstanceID(id))
			continue
		}

		statusLabel := campaign.DisplayInstanceStatus(ci)
		fmt.Printf("Instance %s — %s — %s\n", ids.FormatInstanceID(ci.ID), ci.DisplayGPUBrief(), statusLabel)
		if ci.TerminationReason != "" {
			fmt.Printf("  Terminated: %s\n", ci.DisplayTerminationReason())
		}
		if ci.Status == db.LaunchStatusCompleted {
			switch {
			case ci.ResultsVerified != nil && !*ci.ResultsVerified:
				switch ci.ResultsVerifyDetail {
				case db.ResultsVerifyDetailUploadsIncomplete:
					fmt.Printf("  Results: UNVERIFIED (one or more job uploads were partial/failed)\n")
				case db.ResultsVerifyDetailManifestMissingJobs:
					fmt.Printf("  Results: UNVERIFIED (completion manifest is missing results for some jobs)\n")
				default:
					fmt.Printf("  Results: UNVERIFIED (results upload could not be confirmed)\n")
				}
			case ci.ResultsVerified != nil && *ci.ResultsVerified:
				fmt.Printf("  Results: verified\n")
			}
		}
		fmt.Printf("  Provider: %s\n", ci.Provider)
		if line := formatRentalLine(ci); line != "" {
			fmt.Printf("  Rental:   %s\n", line)
		}

		// Cloud instance info
		providerInstID := ci.EffectiveProviderID()
		var inst *cloud.Instance
		client := cloudClientForDBInstance(ci.Provider)
		useLive := shouldRefreshInstanceLiveState(database, ci, forceLive, noLive)
		if providerInstID != "" {
			if useLive {
				inst, _ = client.ShowInstance(providerInstID)
			}
			if inst != nil && inst.Status != "" {
				fmt.Printf("  Instance: %s (%s)\n", providerInstID, inst.Status)
			} else {
				fmt.Printf("  Instance: %s\n", providerInstID)
			}
		} else {
			fmt.Printf("  Instance: (provisioning...)\n")
		}

		if ci.DataCenter != "" {
			fmt.Printf("  Location: %s\n", ci.DataCenter)
		}

		// Agent version and live status
		var liveUpdate *campaign.InstanceUpdate
		if useLive {
			if r2c, r2err := newR2ClientFromConfig(); r2err == nil && r2c != nil {
				watchCtx, watchCancel := context.WithTimeout(context.Background(), 3*time.Second)
				ch := campaign.WatchInstance(watchCtx, client, database, ci.ID, 100*time.Millisecond, 100*time.Millisecond, r2c)
				if update, ok := <-ch; ok {
					liveUpdate = &update
				}
				watchCancel()
			}
		}
		// Read agent version from DB-cached live state (persists after terminal)
		if liveState, err := db.GetLaunchLiveState(database, ci.ID); err == nil && liveState != nil && liveState.AgentVersion != "" {
			fmt.Printf("  Agent:    %s\n", liveState.AgentVersion)
		}

		obs := terminal.ObserveLaunch(ci, inst, time.Now())
		if obs.Uptime != nil {
			fmt.Printf("  Uptime:   %s\n", *obs.Uptime)
		}
		if obs.Cost != nil {
			fmt.Printf("  Cost:     $%.2f\n", *obs.Cost)
		}
		if obs.Rate != nil {
			fmt.Printf("  Rate:     $%.2f/hr\n", *obs.Rate)
		}
		// Jobs
		jobs, err := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		if err != nil {
			return fmt.Errorf("get instance %s jobs: %w", ids.FormatInstanceID(ci.ID), err)
		}
		if liveUpdate == nil {
			liveUpdate = &campaign.InstanceUpdate{
				Launch:   ci,
				Instance: inst,
				Jobs:     jobs,
			}
		} else {
			liveUpdate.Launch = ci
			if liveUpdate.Instance == nil {
				liveUpdate.Instance = inst
			}
			if len(liveUpdate.Jobs) == 0 {
				liveUpdate.Jobs = jobs
			}
		}
		// Backfill phase/bootstrap from DB-cached live state when the
		// one-shot WatchInstance call didn't return R2 data in time.
		if liveUpdate.InstancePhase == "" || liveUpdate.BootstrapStage == "" {
			if ls, err := db.GetLaunchLiveState(database, ci.ID); err == nil && ls != nil {
				if liveUpdate.InstancePhase == "" {
					liveUpdate.InstancePhase = ls.InstancePhase
				}
				if liveUpdate.BootstrapStage == "" {
					liveUpdate.BootstrapStage = ls.BootstrapStage
				}
				if liveUpdate.PhaseChangedAt == nil && ls.PhaseChangedAt != nil {
					t := time.Unix(*ls.PhaseChangedAt, 0)
					liveUpdate.PhaseChangedAt = &t
				}
			}
		}
		// Backfill adaptive bootstrap timeout for accurate display.
		if liveUpdate.BootstrapTerminateAfter == 0 && ci.Provider != "" {
			if survival, err := db.ComputeBootstrapSurvival(database, ci.Provider); err == nil && survival != nil {
				liveUpdate.BootstrapTerminateAfter = survival.TerminateAfter
				liveUpdate.BootstrapDurations = survival.Durations
			}
		}
		activity := terminal.FormatObservedActivity(*liveUpdate, time.Now())
		if activity.Bootstrap != "" {
			fmt.Printf("  Bootstrap: %s\n", activity.Bootstrap)
		}
		if activity.Phase != "" {
			fmt.Printf("  Phase:    %s\n", activity.Phase)
		}
		if liveUpdate != nil && liveUpdate.HeartbeatAge > 0 {
			fmt.Printf("  Heartbeat: %s ago\n", liveUpdate.HeartbeatAge.Truncate(time.Second))
		}
		if label := campaign.TerminationIntentLabel(ci.TerminationIntent); label != "" {
			fmt.Printf("  Cleanup: %s\n", label)
			if detail := campaign.TerminationIntentDetail(ci.TerminationIntent); detail != "" {
				fmt.Printf("  Cleanup status: %s\n", detail)
			}
		}
		if detail := ci.CordonDetail(); detail != "" {
			fmt.Printf("  Cordoned: %s\n", detail)
			if ci.CordonedAt != nil {
				fmt.Printf("  Cordoned at: %s\n", time.Unix(*ci.CordonedAt, 0).Format("01/02 15:04"))
			}
		}

		outcomes, _ := db.GetAttemptOutcomesByLaunch(database, ci.ID)
		if len(jobs) > 0 {
			fmt.Printf("  Jobs:\n")
			printInstanceJob := func(j *db.Job) {
				displayStatus := campaign.AttemptDisplayStatus(j, outcomes)
				desc := j.Description
				if desc == "" {
					desc = campaign.TruncateCommand(j.EffectiveCommand(), 50)
				}
				fmt.Printf("    %-6s %-12s %s\n", ids.FormatJobID(j.ID), displayStatus, desc)
				if campaign.IsJobTerminal(displayStatus) {
					if timings, err := db.GetJobPhaseTimings(database, j.ID); err == nil {
						if summary := terminal.FormatUploadSummary(timings); summary != "" {
							fmt.Printf("             uploads: %s\n", summary)
						}
					}
				}
				if displayStatus == db.StatusFailed || displayStatus == db.AttemptOutcomeFailed || displayStatus == db.AttemptOutcomeOrphaned {
					excerpt := diagnosisSummaryFromJSON(j.ErrorDiagnosis)
					if excerpt == "" && displayStatus != db.AttemptOutcomeOrphaned {
						excerpt = terminal.ReadCachedJobFailureExcerpt(j.ID)
					}
					if excerpt == "" {
						excerpt = util.Truncate(joinNonEmpty([]string{j.FailureReason, j.ErrorMessage, j.ErrorDiagnosis}, " | "), 180)
					}
					if excerpt != "" {
						fmt.Printf("             failure: %s\n", excerpt)
					}
				}
			}
			for _, j := range jobs {
				printInstanceJob(j)
			}
		}

		// Previous instances (replacement chain)
		if donors := walkReplacementChain(database, ci); len(donors) > 0 {
			line := terminal.FormatPreviousInstanceLine(donors, time.Now())
			if line != "" {
				fmt.Println(line)
			}
		}
	}
	return nil
}

func instanceStatusSyncMode(cmd *cobra.Command) (forceLive bool, noLive bool) {
	if cmd != nil && isTopLevelStatusCommand(cmd) {
		return statusSync, statusNoSync
	}
	if cmd != nil {
		switch cmd.Name() {
		case "info", "show":
			if cmd.Parent() == rootCmd {
				return jobInfoSync, jobInfoNoSync
			}
		}
	}
	return instanceStatusSync, instanceStatusNoSync
}

func shouldRefreshInstanceLiveState(database *sql.DB, ci *db.Launch, forceLive, noLive bool) bool {
	if ci == nil || campaign.IsInstanceTerminal(ci.Status) || noLive {
		return false
	}
	if forceLive {
		return true
	}
	if !daemonLiveFunc() {
		return true
	}
	return !syncTargetFresh(database, db.CloudSyncTargetName, time.Now())
}

func printInventoryTargetStatus(database *sql.DB, host string) (bool, error) {
	if strings.TrimSpace(host) == "" {
		return false, nil
	}
	targets, err := db.ListExecutionTargets(database)
	if err != nil {
		return true, err
	}
	for _, target := range targets {
		if target == nil || target.Kind != db.ExecutionTargetInventoryHost || target.Host != host {
			continue
		}
		statusStr := string(target.Status)
		if target.Cordoned {
			statusStr += " [cordoned]"
		}
		fmt.Printf("Target %s — %s\n", target.Host, statusStr)
		if target.CordonReason != "" {
			fmt.Printf("  Cordon: %s\n", target.CordonReason)
		}
		if target.CurrentJobID != nil {
			fmt.Printf("  Current job: %s\n", ids.FormatJobID(*target.CurrentJobID))
		}
		fmt.Printf("  Queue depth: %d\n", target.QueueDepth)
		if target.LastObservedAt != nil {
			fmt.Printf("  Last observed: %s\n", time.Unix(*target.LastObservedAt, 0).Format(time.RFC3339))
		}
		return true, nil
	}
	return false, nil
}

// walkReplacementChain follows ReplacedInstanceID links to build a predecessor chain.
func walkReplacementChain(database *sql.DB, ci *db.Launch) []*db.Launch {
	return terminal.CollectReplacementChain(ci, func(id int64) *db.Launch {
		predecessor, _ := db.GetLaunch(database, id)
		return predecessor
	})
}

// formatRentalLine renders the rental-type segment for `weft info`. Returns
// "" for pre-migration rows where InstanceType was never recorded.
func formatRentalLine(ci *db.Launch) string {
	switch ci.InstanceType {
	case cloud.InstanceTypeInterruptible:
		if ci.MaxBidPriceCents != nil {
			return fmt.Sprintf("interruptible · max bid $%.2f/hr", float64(*ci.MaxBidPriceCents)/100)
		}
		return "interruptible"
	case cloud.InstanceTypeOnDemand:
		return "on-demand"
	}
	return ""
}

func runInstanceTerminate(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var instanceIDs []int64
	for _, arg := range args {
		id, err := ids.ParseInstanceID(arg)
		if err != nil {
			return fmt.Errorf("invalid instance ID %q: %w", arg, err)
		}
		instanceIDs = append(instanceIDs, id)
	}

	terminated, errors := terminateInstancesParallel(database, instanceIDs)
	for _, e := range errors {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", e)
	}
	if terminated > 0 {
		fmt.Printf("Terminated %d instance(s)\n", terminated)
	}
	if len(errors) > 0 {
		return fmt.Errorf("%d instance(s) had errors", len(errors))
	}
	return nil
}

// terminateInstancesParallel terminates multiple cloud instances in parallel.
func terminateInstancesParallel(database *sql.DB, instanceIDs []int64) (int, []error) {
	return orchestration.TerminateInstancesParallel(database, instanceIDs)
}

func runInstanceSSH(cmd *cobra.Command, args []string) error {
	id, err := ids.ParseInstanceID(args[0])
	if err != nil {
		return fmt.Errorf("invalid instance ID %q: %w", args[0], err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	ci, err := db.GetLaunch(database, id)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}
	if ci == nil {
		return fmt.Errorf("instance %s not found", ids.FormatInstanceID(id))
	}

	providerInstID := ci.EffectiveProviderID()

	if providerInstID == "" {
		fmt.Println("Waiting for instance...")

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		for providerInstID == "" {
			select {
			case <-ctx.Done():
				return fmt.Errorf("timed out waiting for instance")
			case <-time.After(2 * time.Second):
			}
			ci, err = db.GetLaunch(database, id)
			if err != nil {
				return fmt.Errorf("get instance: %w", err)
			}
			providerInstID = ci.EffectiveProviderID()
		}
	}

	// Get cloud instance info
	client := cloudClientForDBInstance(ci.Provider)

	// Wait for instance to be ready
	fmt.Println("Waiting for instance...")
	inst, err := client.WaitReady(providerInstID, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("wait for instance: %w", err)
	}

	fmt.Println("ready.")

	if instanceSSHPrint {
		fmt.Println(campaign.FormatSSHCommand(inst))
		return nil
	}

	return execSSH(inst)
}

func runInstanceSubmit(cmd *cobra.Command, args []string) error {
	instanceID, err := ids.ParseInstanceID(args[0])
	if err != nil {
		return fmt.Errorf("invalid instance ID %q: %w", args[0], err)
	}
	jobID, err := ids.ParseJobID(args[1])
	if err != nil {
		return fmt.Errorf("invalid job ID %q: %w", args[1], err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if _, err := getGraceInstance(database, instanceID); err != nil {
		return err
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}

	// Override command if specified via CLI flag
	if instanceSubmitCommand != "" {
		job.Command = instanceSubmitCommand
	}

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return err
	}

	ctx := context.Background()
	fmt.Printf("Re-syncing sources and submitting job %s to instance %s...\n", ids.FormatJobID(jobID), ids.FormatInstanceID(instanceID))

	if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, instanceID, []*db.Job{job}); err != nil {
		return err
	}

	// Persist command override to DB (SubmitJobsToInstance resets status to queued first)
	if instanceSubmitCommand != "" {
		if err := db.UpdateJobCommand(database, jobID, instanceSubmitCommand); err != nil {
			return fmt.Errorf("persist command override: %w", err)
		}
	}

	fmt.Printf("Job %s resubmitted to instance %s.\n", ids.FormatJobID(jobID), ids.FormatInstanceID(instanceID))
	fmt.Println("Use 'weft campaign watch' or 'weft instance status' to monitor progress.")
	return nil
}

func runInstanceExtend(cmd *cobra.Command, args []string) error {
	instanceID, err := ids.ParseInstanceID(args[0])
	if err != nil {
		return fmt.Errorf("invalid instance ID %q: %w", args[0], err)
	}

	duration := 15 * time.Minute
	if len(args) > 1 {
		d, err := time.ParseDuration(args[1])
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", args[1], err)
		}
		duration = d
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if _, err := getGraceInstance(database, instanceID); err != nil {
		return err
	}

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return err
	}

	ctx := context.Background()
	if _, err := controlplane.SendGraceExtend(ctx, r2Client, instanceID, duration); err != nil {
		return fmt.Errorf("write extend signal: %w", err)
	}

	newDeadline := time.Now().Add(duration).Unix()
	if err := db.ExtendLaunchGrace(database, instanceID, newDeadline); err != nil {
		return fmt.Errorf("update DB: %w", err)
	}

	fmt.Printf("Grace period for instance %s extended by %s.\n", ids.FormatInstanceID(instanceID), duration)
	return nil
}

func runInstanceRelease(cmd *cobra.Command, args []string) error {
	instanceID, err := ids.ParseInstanceID(args[0])
	if err != nil {
		return fmt.Errorf("invalid instance ID %q: %w", args[0], err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if _, err := getGraceInstance(database, instanceID); err != nil {
		return err
	}

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		return err
	}

	ctx := context.Background()
	if _, err := controlplane.SendGraceRelease(ctx, r2Client, instanceID); err != nil {
		return fmt.Errorf("write release signal: %w", err)
	}

	if err := markReleasedInstanceFailed(database, instanceID); err != nil {
		return err
	}

	fmt.Printf("Instance %s released. It will self-destruct shortly.\n", ids.FormatInstanceID(instanceID))
	return nil
}

func runInstanceCordon(cmd *cobra.Command, args []string) error {
	return setInstanceCordon(args, true, instanceCordonReason)
}

func runInstanceUncordon(cmd *cobra.Command, args []string) error {
	return setInstanceCordon(args, false, "")
}

func setInstanceCordon(args []string, cordoned bool, reason string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	for _, arg := range args {
		id, err := ids.ParseInstanceID(arg)
		if err != nil {
			if err := db.SetInventoryExecutionTargetCordoned(database, arg, cordoned, reason); err != nil {
				return fmt.Errorf("set cordon on host %s: %w", arg, err)
			}
			if !cordoned {
				fmt.Printf("Host %s uncordoned.\n", arg)
				continue
			}
			suffix := ""
			if reason != "" {
				suffix = " (" + reason + ")"
			}
			fmt.Printf("Host %s cordoned%s. Autopilot will not route new jobs to it.\n", arg, suffix)
			continue
		}
		err = db.SetLaunchCordoned(database, id, cordoned, reason)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("instance %s not found", ids.FormatInstanceID(id))
		}
		if err != nil {
			return fmt.Errorf("set cordon on %s: %w", ids.FormatInstanceID(id), err)
		}
		if !cordoned {
			fmt.Printf("Instance %s uncordoned.\n", ids.FormatInstanceID(id))
			continue
		}
		suffix := ""
		if reason != "" {
			suffix = " (" + reason + ")"
		}
		fmt.Printf("Instance %s cordoned%s. Autopilot will not route new jobs to it.\n", ids.FormatInstanceID(id), suffix)
	}
	return nil
}

func markReleasedInstanceFailed(database *sql.DB, instanceID int64) error {
	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonJobFailure); err != nil {
		return fmt.Errorf("update DB: %w", err)
	}
	if _, err := db.NormalizeTerminalLaunchJobs(database, instanceID); err != nil {
		return fmt.Errorf("normalize jobs: %w", err)
	}
	return nil
}

func execSSH(inst *cloud.Instance) error {
	args := append([]string{"ssh"}, cloud.InstanceSSHArgs(inst)...)
	target := cloud.InstanceSSHTarget(inst)
	args = append(args, target)
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found: %w", err)
	}
	sshaudit.Log(sshaudit.KindInteractive, target, 0)
	return syscall.Exec(sshPath, args, os.Environ())
}

// joinNonEmpty joins non-empty trimmed strings with sep. Empty inputs are
// dropped so the output never contains adjacent separators with nothing
// between them (e.g. "foo |  | bar").
func joinNonEmpty(parts []string, sep string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, sep)
}
