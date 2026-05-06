package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/spf13/cobra"
)

// Exit codes for `autopilot status --quiet`. Always exits 0 in non-quiet mode
// (a successful status read is not an error), so shells can `if weft autopilot
// status --quiet; then ...` for fast branching without parsing JSON.
const (
	autopilotExitIdle    = 0
	autopilotExitRunning = 10
	autopilotExitStale   = 11
	autopilotExitPaused  = 12
)

type autopilotStateName string

const (
	stateIdle    autopilotStateName = "idle"
	stateRunning autopilotStateName = "running"
	stateStale   autopilotStateName = "stale"
	statePaused  autopilotStateName = "paused"
	stateNever   autopilotStateName = "never"
)

var (
	autopilotStatusJSON  bool
	autopilotStatusQuiet bool
	autopilotPauseReason string
	autopilotPauseBy     string
)

var autopilotCmd = &cobra.Command{
	Use:   "autopilot",
	Short: "Inspect and control the autopilot",
	Long: `The autopilot drives auto-placement and auto-launch decisions from any TUI
that has auto-mode enabled.

This command exposes the singleton autopilot state stored in the local
database: whether the autopilot is paused, and which process (if any) is
currently driving a pass.

Pause is sticky across restarts. While paused, all TUIs and other runners
skip their autopilot work, so it's safe for an external agent to launch
instances or restart jobs by hand without racing the autopilot.`,
}

var autopilotStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show whether the autopilot is running, idle, or paused",
	Long: `Show the singleton autopilot state.

States:
  idle      — no pass in flight; safe for external automation
  running   — a TUI or other runner is currently driving a pass
  stale     — a runner claimed the slot but its heartbeat aged out
              (likely crashed; the next runner will reclaim)
  paused    — autopilot is globally paused; no runner will act

In non-quiet mode this always exits 0 on a successful read. Use --quiet
for fast shell branching without parsing JSON:

  if weft autopilot status --quiet; then
      # idle: it's safe to launch / restart manually
  fi

Quiet exit codes: 0=idle, 10=running, 11=stale, 12=paused.`,
	Args: cobra.NoArgs,
	RunE: runAutopilotStatus,
}

var autopilotPauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Pause the autopilot (sticky across restarts)",
	Long: `Set the sticky paused flag. While paused, all autopilot runners (list TUI,
watch TUI, future daemon) skip their passes — no auto-placement, no
auto-launch, no automatic relaunch of orphaned jobs.

Pause does not interrupt a pass that is already in flight. If you need
an immediate halt, wait for the active runner to finish (use
"weft autopilot status" to observe) before issuing manual commands.`,
	Args: cobra.NoArgs,
	RunE: runAutopilotPause,
}

var autopilotResumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Resume the autopilot",
	Args:  cobra.NoArgs,
	RunE:  runAutopilotResume,
}

var autopilotBudgetCmd = &cobra.Command{
	Use:   "budget",
	Short: "Inspect and control the autopilot's spend budget",
	Long: `Manage the daily-window spend cap that the runaway breaker uses
to pause unattended relaunches when no jobs are completing.`,
}

var autopilotBudgetResetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Clear the global runaway-breaker trip",
	Long: `Insert a global resume event for the runaway breaker (campaign_id=NULL,
project=<all>) so the autopilot stops blocking unplaced jobs after a
no-progress spend trip.

Use this when "weft jobs list" reports
  blocked: paused: repeated launch failures without progress
on jobs that aren't tied to a specific campaign. Per-campaign trips are
cleared with "weft campaign safety resume --campaign <id>".`,
	Args: cobra.NoArgs,
	RunE: runAutopilotBudgetReset,
}

var (
	autopilotBlockedJSON    bool
	autopilotBlockedUnblock bool
)

var autopilotBlockedCmd = &cobra.Command{
	Use:   "blocked",
	Short: "Show currently-tripped runaway-breaker scopes and the jobs they're holding",
	Long: `List every runaway-breaker scope whose latest trip event has not been
cleared by a later resume event, with the trip metrics (chain length,
orphaned-attempt count, spend, window) and the queued jobs that share
the scope.

This is the focused diagnostic for "why is wj_X paused?". The trip
metrics tell you which threshold fired, which is the difference between
"three failures in a row, just unlucky" and "no completions in 24
hours, something is structurally wrong".

Use --unblock to clear all global trips and resume the autopilot in one
shot. Per-campaign trips are cleared with their campaign-scoped resume
flow; this CLI only manages the global scope (campaign=<none>,
project=<all>).`,
	Args: cobra.NoArgs,
	RunE: runAutopilotBlocked,
}

func init() {
	rootCmd.AddCommand(autopilotCmd)
	autopilotCmd.AddCommand(autopilotStatusCmd)
	autopilotCmd.AddCommand(autopilotPauseCmd)
	autopilotCmd.AddCommand(autopilotResumeCmd)
	autopilotCmd.AddCommand(autopilotBudgetCmd)
	autopilotCmd.AddCommand(autopilotBlockedCmd)
	autopilotBudgetCmd.AddCommand(autopilotBudgetResetCmd)

	autopilotBlockedCmd.Flags().BoolVar(&autopilotBlockedJSON, "json", false, "Emit machine-readable JSON")
	autopilotBlockedCmd.Flags().BoolVar(&autopilotBlockedUnblock, "unblock", false, "Clear the global runaway-breaker trip after listing")

	autopilotStatusCmd.Flags().BoolVar(&autopilotStatusJSON, "json", false, "Emit machine-readable JSON")
	autopilotStatusCmd.Flags().BoolVar(&autopilotStatusQuiet, "quiet", false,
		"Suppress output and exit non-zero when autopilot is busy/stale/paused (0=idle, 10=running, 11=stale, 12=paused)")

	autopilotPauseCmd.Flags().StringVar(&autopilotPauseReason, "reason", "", "Free-text reason recorded with the pause")
	autopilotPauseCmd.Flags().StringVar(&autopilotPauseBy, "by", "", "Who is pausing (defaults to $USER)")
}

// autopilotStateView is the JSON shape returned by `weft autopilot status --json`.
// Stable contract — agents may parse this. Timestamps are RFC3339.
type autopilotStateView struct {
	State              autopilotStateName `json:"state"`
	Paused             bool               `json:"paused"`
	PausedAt           *string            `json:"paused_at,omitempty"`
	PausedBy           string             `json:"paused_by,omitempty"`
	PausedReason       string             `json:"paused_reason,omitempty"`
	ActiveRunnerPID    int                `json:"active_runner_pid,omitempty"`
	ActiveRunnerLabel  string             `json:"active_runner_label,omitempty"`
	ActiveRunnerHost   string             `json:"active_runner_host,omitempty"`
	PassStartedAt      *string            `json:"pass_started_at,omitempty"`
	PassAgeSeconds     int64              `json:"pass_age_seconds,omitempty"`
	HeartbeatAt        *string            `json:"heartbeat_at,omitempty"`
	HeartbeatAgeS      int64              `json:"heartbeat_age_seconds,omitempty"`
	StaleAfterSeconds  int64              `json:"stale_after_seconds"`
	LastPassFinishedAt *string            `json:"last_pass_finished_at,omitempty"`
	LastPassDurationMS int64              `json:"last_pass_duration_ms,omitempty"`
	LastPassSummary    string             `json:"last_pass_summary,omitempty"`
	LastPassError      string             `json:"last_pass_error,omitempty"`
	OrphanStreaks      []orphanStreakView `json:"orphan_streaks,omitempty"`
}

// orphanStreakView reports a job stuck in a launch-orphan loop. Surfaces the
// case where every recent rental for a given job died before the agent ever
// reported (dud-Vast, runpod ssh-not-ready, etc) so the user notices the
// money/time burn even though the runaway breaker doesn't trip on orphans.
type orphanStreakView struct {
	JobID       string `json:"job_id"`
	OrphanCount int    `json:"orphan_count"`
	Project     string `json:"project,omitempty"`
	Description string `json:"description,omitempty"`
}

// orphanStreakThreshold is the minimum number of orphan attempts (since the
// last non-orphan outcome) before a job appears in autopilot status warnings.
const orphanStreakThreshold = 5

func buildAutopilotStateView(state *db.AutopilotState) autopilotStateView {
	now := time.Now()
	staleAfter := orchestration.AutopilotPassStaleAfter
	view := autopilotStateView{
		StaleAfterSeconds: int64(staleAfter / time.Second),
	}
	if state == nil {
		view.State = stateNever
		return view
	}
	view.Paused = state.Paused
	view.PausedBy = state.PausedBy
	view.PausedReason = state.PausedReason
	view.ActiveRunnerPID = state.ActiveRunnerPID
	view.ActiveRunnerLabel = state.ActiveRunnerLabel
	view.ActiveRunnerHost = state.ActiveRunnerHost
	view.LastPassDurationMS = state.LastPassDurationMS
	view.LastPassSummary = state.LastPassSummary
	view.LastPassError = state.LastPassError
	if !state.PausedAt.IsZero() {
		s := state.PausedAt.Format(time.RFC3339)
		view.PausedAt = &s
	}
	if !state.PassStartedAt.IsZero() {
		s := state.PassStartedAt.Format(time.RFC3339)
		view.PassStartedAt = &s
		view.PassAgeSeconds = int64(now.Sub(state.PassStartedAt) / time.Second)
	}
	if !state.LastHeartbeat.IsZero() {
		s := state.LastHeartbeat.Format(time.RFC3339)
		view.HeartbeatAt = &s
		view.HeartbeatAgeS = int64(now.Sub(state.LastHeartbeat) / time.Second)
	}
	if !state.LastPassFinishedAt.IsZero() {
		s := state.LastPassFinishedAt.Format(time.RFC3339)
		view.LastPassFinishedAt = &s
	}

	switch {
	case state.Paused:
		view.State = statePaused
	case state.IsActive(now, staleAfter):
		view.State = stateRunning
	case state.IsStale(now, staleAfter):
		view.State = stateStale
	case state.LastPassFinishedAt.IsZero() && state.PassStartedAt.IsZero():
		view.State = stateNever
	default:
		view.State = stateIdle
	}
	return view
}

func autopilotStateExitCode(view autopilotStateView) int {
	switch view.State {
	case stateRunning:
		return autopilotExitRunning
	case stateStale:
		return autopilotExitStale
	case statePaused:
		return autopilotExitPaused
	default:
		return autopilotExitIdle
	}
}

func runAutopilotStatus(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	state, err := db.LoadAutopilotState(database)
	if err != nil {
		return fmt.Errorf("load autopilot state: %w", err)
	}
	view := buildAutopilotStateView(state)
	if streaks, err := db.JobsWithOrphanStreaks(database, orphanStreakThreshold); err == nil {
		for _, s := range streaks {
			view.OrphanStreaks = append(view.OrphanStreaks, orphanStreakView{
				JobID:       ids.FormatJobID(s.JobID),
				OrphanCount: s.OrphanCount,
				Project:     s.Project,
				Description: s.Description,
			})
		}
	}

	if autopilotStatusQuiet {
		exitCode := autopilotStateExitCode(view)
		database.Close()
		os.Exit(exitCode)
		return nil
	}

	if autopilotStatusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}

	fmt.Println(formatAutopilotStatusText(view))
	return nil
}

func formatAutopilotStatusText(view autopilotStateView) string {
	var b strings.Builder
	switch view.State {
	case stateRunning:
		fmt.Fprintf(&b, "autopilot: RUNNING — %s (pid %d on %s), pass started %s ago, heartbeat %s ago",
			defaultStr(view.ActiveRunnerLabel, "unknown"),
			view.ActiveRunnerPID,
			defaultStr(view.ActiveRunnerHost, "?"),
			db.FormatDuration(view.PassAgeSeconds),
			db.FormatDuration(view.HeartbeatAgeS))
	case stateStale:
		fmt.Fprintf(&b, "autopilot: STALE — %s (pid %d on %s) claimed pass but heartbeat is %s old (>%ds); next runner will reclaim",
			defaultStr(view.ActiveRunnerLabel, "unknown"),
			view.ActiveRunnerPID,
			defaultStr(view.ActiveRunnerHost, "?"),
			db.FormatDuration(view.HeartbeatAgeS),
			view.StaleAfterSeconds)
	case statePaused:
		fmt.Fprint(&b, "autopilot: PAUSED")
		if view.PausedBy != "" {
			fmt.Fprintf(&b, " by %s", view.PausedBy)
		}
		if view.PausedAt != nil {
			fmt.Fprintf(&b, " at %s", *view.PausedAt)
		}
		if view.PausedReason != "" {
			fmt.Fprintf(&b, " — %s", view.PausedReason)
		}
	case stateNever:
		fmt.Fprint(&b, "autopilot: idle (never run)")
	default:
		fmt.Fprint(&b, "autopilot: idle")
		if view.LastPassFinishedAt != nil {
			fmt.Fprintf(&b, " — last pass finished %s", *view.LastPassFinishedAt)
		}
		if view.LastPassSummary != "" {
			fmt.Fprintf(&b, " (%s)", view.LastPassSummary)
		}
		if view.LastPassError != "" {
			fmt.Fprintf(&b, " [last error: %s]", view.LastPassError)
		}
	}
	for _, s := range view.OrphanStreaks {
		desc := s.Description
		if len(desc) > 60 {
			desc = desc[:57] + "..."
		}
		fmt.Fprintf(&b, "\n  warning: %s stuck in launch-orphan loop — %d consecutive orphans", s.JobID, s.OrphanCount)
		if s.Project != "" {
			fmt.Fprintf(&b, " (%s)", s.Project)
		}
		if desc != "" {
			fmt.Fprintf(&b, ": %s", desc)
		}
	}
	return b.String()
}

func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func runAutopilotPause(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	by := autopilotPauseBy
	if by == "" {
		if u, err := user.Current(); err == nil {
			by = u.Username
		}
	}
	state, err := db.PauseAutopilot(database, by, autopilotPauseReason)
	if err != nil {
		return fmt.Errorf("pause autopilot: %w", err)
	}
	oplog.Log("autopilot.pause",
		oplog.WithDetailf("by=%s reason=%q", by, autopilotPauseReason))

	fmt.Println("Autopilot paused.")
	if state != nil && !state.PassStartedAt.IsZero() && state.IsActive(time.Now(), orchestration.AutopilotPassStaleAfter) {
		fmt.Printf("Note: a pass is currently in flight (pid %d, %s). Pause takes effect for the *next* pass.\n",
			state.ActiveRunnerPID, defaultStr(state.ActiveRunnerLabel, "unknown"))
		fmt.Println("Use `weft autopilot status` to confirm the active pass has finished before launching manually.")
	}
	return nil
}

func runAutopilotResume(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if _, err := db.ResumeAutopilot(database); err != nil {
		return fmt.Errorf("resume autopilot: %w", err)
	}
	oplog.Log("autopilot.resume")
	fmt.Println("Autopilot resumed.")
	return nil
}

func runAutopilotBudgetReset(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	if err := campaign.ResetGlobalRunawayBreaker(database, "CLI"); err != nil {
		return fmt.Errorf("record resume event: %w", err)
	}
	oplog.Log("autopilot.budget.reset")
	fmt.Println("Runaway-breaker reset for global scope (campaign=<none>, project=<all>).")
	return nil
}

// blockedScopeView is the JSON shape for `weft autopilot blocked --json`.
// Stable contract: ops scripts may parse this to alert on long-tripped
// scopes or to enumerate affected jobs.
type blockedScopeView struct {
	Scope      string   `json:"scope"`
	CampaignID int64    `json:"campaign_id,omitempty"`
	Project    string   `json:"project"`
	TrippedAt  string   `json:"tripped_at"`
	AgeSeconds int64    `json:"age_seconds"`
	Chain      int      `json:"chain"`
	Orphaned   int      `json:"orphaned"`
	SpendCents int      `json:"spend_cents"`
	Window     string   `json:"window"`
	Jobs       []string `json:"jobs,omitempty"`
}

func runAutopilotBlocked(cmd *cobra.Command, args []string) error {
	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	infos, err := campaign.LookupActiveRunawayBreakers(database)
	if err != nil {
		return err
	}

	views := make([]blockedScopeView, 0, len(infos))
	for _, info := range infos {
		jobIDs, err := campaign.JobsBlockedByBreaker(database, info)
		if err != nil {
			return err
		}
		jobLabels := make([]string, 0, len(jobIDs))
		for _, id := range jobIDs {
			jobLabels = append(jobLabels, fmt.Sprintf("wj%d", id))
		}
		views = append(views, blockedScopeView{
			Scope:      info.ScopeLabel(),
			CampaignID: info.CampaignID,
			Project:    info.Project,
			TrippedAt:  info.TrippedAt.Format(time.RFC3339),
			AgeSeconds: int64(time.Since(info.TrippedAt).Seconds()),
			Chain:      info.Chain,
			Orphaned:   info.Orphaned,
			SpendCents: info.SpendCents,
			Window:     info.Window.String(),
			Jobs:       jobLabels,
		})
	}

	if autopilotBlockedJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"scopes": views}); err != nil {
			return err
		}
	} else if len(views) == 0 {
		fmt.Println("No active runaway-breaker trips.")
	} else {
		for _, v := range views {
			age := time.Duration(v.AgeSeconds) * time.Second
			fmt.Printf("%s\n", v.Scope)
			fmt.Printf("  tripped %s ago (%s)\n", age.Truncate(time.Second), v.TrippedAt)
			fmt.Printf("  metrics: chain=%d orphaned=%d spend=$%.2f window=%s\n",
				v.Chain, v.Orphaned, float64(v.SpendCents)/100.0, v.Window)
			if len(v.Jobs) > 0 {
				fmt.Printf("  jobs (%d): %s\n", len(v.Jobs), strings.Join(v.Jobs, ", "))
			} else {
				fmt.Printf("  jobs: (none currently queued in scope)\n")
			}
			fmt.Println()
		}
		if !autopilotBlockedUnblock {
			fmt.Println("Reset with: weft autopilot budget reset")
			fmt.Println("Or rerun:   weft autopilot blocked --unblock")
		}
	}

	if autopilotBlockedUnblock {
		// Open writable DB handle; the read-only one above can't insert events.
		dbw, err := db.Open()
		if err != nil {
			return fmt.Errorf("open database for unblock: %w", err)
		}
		defer dbw.Close()
		if err := campaign.ResetGlobalRunawayBreaker(dbw, "CLI"); err != nil {
			return fmt.Errorf("reset breaker: %w", err)
		}
		oplog.Log("autopilot.blocked.unblock")
		fmt.Println("Runaway-breaker reset for global scope (campaign=<none>, project=<all>).")
	}
	return nil
}
