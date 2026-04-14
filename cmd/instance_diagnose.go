package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

var instanceDiagnoseCmd = &cobra.Command{
	Use:   "diagnose <instance-id>",
	Short: "Deep diagnosis of a cloud instance with timeline and survival analysis",
	Long: `Shows a detailed diagnostic report for a single cloud instance, including:
- Root cause analysis
- Chronological timeline of all events (as offsets from launch)
- Bootstrap and setup survival analysis with adaptive thresholds
- Per-job diagnosis with phase durations and GPU metrics`,
	Args: usageArgs(cobra.ExactArgs(1)),
	RunE: runInstanceDiagnose,
}

// instanceDiagnoseReport holds all data needed for the diagnostic output.
type instanceDiagnoseReport struct {
	Instance          *db.Launch
	Jobs              []*db.Job
	Outcomes          map[int64]string
	PhaseTimings      map[int64]*db.JobPhaseTimings
	LifecycleEvents   []db.LifecycleEvent
	BootstrapSurvival *db.BootstrapSurvival
	SetupSurvival     *db.SetupSurvival
	LivePhaseRaw      string
	LivePhase         string
	LivePhaseChanged  *time.Time
	Obs               terminal.CloudInstanceObservability
	Diagnosis         instanceDiagnosis
	Timeline          []timelineEntry
}

// timelineEntry is a single event in the chronological timeline.
type timelineEntry struct {
	Timestamp int64
	Label     string
}

func runInstanceDiagnose(_ *cobra.Command, args []string) error {
	instanceID, err := ids.ParseInstanceID(args[0])
	if err != nil {
		return usageErrorf("invalid instance ID %q", args[0])
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	inst, err := db.GetLaunch(database, instanceID)
	if err != nil {
		return fmt.Errorf("get instance: %w", err)
	}
	if inst == nil {
		return fmt.Errorf("instance %s not found", ids.FormatInstanceID(instanceID))
	}

	// Refine termination reason if it's a generic job_failure.
	if inst.TerminationReason == db.TerminationReasonJobFailure {
		_ = db.RefineInstanceTerminationReason(database, inst.ID)
		if refreshed, err := db.GetLaunch(database, inst.ID); err == nil && refreshed != nil {
			inst = refreshed
		}
	}

	jobs, err := db.GetLaunchJobsIncludingAttempts(database, inst.ID)
	if err != nil {
		return fmt.Errorf("get jobs: %w", err)
	}

	outcomes, err := db.GetAttemptOutcomesByLaunch(database, inst.ID)
	if err != nil {
		return fmt.Errorf("get attempt outcomes: %w", err)
	}

	timings := make(map[int64]*db.JobPhaseTimings)
	for _, j := range jobs {
		t, err := db.GetJobPhaseTimings(database, j.ID)
		if err != nil || t == nil {
			continue
		}
		// Filter out timings from other instances' attempts.
		// Timings before launch belong to a prior attempt; timings after ended_at belong to a later one.
		if t.WrapperStart != nil {
			if inst.LaunchedAt != nil && *t.WrapperStart < *inst.LaunchedAt {
				continue
			}
			if inst.EndedAt != nil && *t.WrapperStart > *inst.EndedAt {
				continue
			}
		}
		timings[j.ID] = t
	}

	events, err := db.ListLifecycleEvents(database, db.LifecycleEventFilter{LaunchID: inst.ID})
	if err != nil {
		return fmt.Errorf("get lifecycle events: %w", err)
	}

	bootstrapSurvival, _ := db.ComputeBootstrapSurvival(database, inst.Provider)

	var setupSurvival *db.SetupSurvival
	for _, j := range jobs {
		if j.Command != "" {
			setupSurvival, _ = db.ComputeSetupSurvival(database, j.Command, j.WorkingDir)
			break
		}
	}

	obs := terminal.ObserveLaunch(inst, nil, time.Now())
	diagnosis := buildInstanceDiagnosis(inst, jobs, outcomes)
	timeline := buildTimeline(inst, jobs, timings, events)

	var livePhase string
	var livePhaseChanged *time.Time
	if liveState, err := db.GetLaunchLiveState(database, inst.ID); err == nil && liveState != nil {
		livePhase = strings.TrimSpace(liveState.InstancePhase)
		if liveState.PhaseChangedAt != nil {
			ts := time.Unix(*liveState.PhaseChangedAt, 0)
			livePhaseChanged = &ts
		}
	}
	livePhaseRaw := ""
	if r2Client, err := newR2ClientFromConfig(); err == nil && r2Client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if data, getErr := r2Client.GetObject(ctx, r2keys.InstancePhase(inst.ID)); getErr == nil {
			livePhaseRaw = strings.TrimSpace(string(data))
		}
		cancel()
	}

	report := &instanceDiagnoseReport{
		Instance:          inst,
		Jobs:              jobs,
		Outcomes:          outcomes,
		PhaseTimings:      timings,
		LifecycleEvents:   events,
		BootstrapSurvival: bootstrapSurvival,
		SetupSurvival:     setupSurvival,
		LivePhaseRaw:      livePhaseRaw,
		LivePhase:         livePhase,
		LivePhaseChanged:  livePhaseChanged,
		Obs:               obs,
		Diagnosis:         diagnosis,
		Timeline:          timeline,
	}

	fmt.Print(formatInstanceDiagnoseReport(report))
	return nil
}

// buildTimeline constructs a chronological list of events from all sources.
func buildTimeline(inst *db.Launch, jobs []*db.Job, timings map[int64]*db.JobPhaseTimings, events []db.LifecycleEvent) []timelineEntry {
	var entries []timelineEntry

	add := func(ts *int64, label string) {
		if ts != nil && *ts > 0 {
			entries = append(entries, timelineEntry{Timestamp: *ts, Label: label})
		}
	}

	addVal := func(ts int64, label string) {
		if ts > 0 {
			entries = append(entries, timelineEntry{Timestamp: ts, Label: label})
		}
	}

	addVal(inst.CreatedAt, "created")
	add(inst.ReadyAt, "provider ready")
	add(inst.LaunchedAt, "launched")

	for _, j := range jobs {
		t := timings[j.ID]
		if t == nil {
			continue
		}
		prefix := fmt.Sprintf("job %d", j.ID)
		add(t.WrapperStart, prefix+": wrapper started")
		add(t.SetupStart, prefix+": setup started")
		add(t.SetupEnd, prefix+": setup ended")
		add(t.RunStart, prefix+": run started")
		add(t.RunEnd, prefix+": run ended")
		add(t.UploadStart, prefix+": upload started")
		add(t.UploadEnd, prefix+": upload ended")
	}

	for _, e := range events {
		label := e.EventKind
		if e.Detail != "" {
			label += ": " + e.Detail
		}
		addVal(e.OccurredAt, label)
	}

	add(inst.EndedAt, terminationLabel(inst))

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Timestamp < entries[j].Timestamp
	})
	return entries
}

func terminationLabel(inst *db.Launch) string {
	if inst.TerminationReason != "" && inst.TerminationReason != db.TerminationReasonCompleted {
		return "ended (" + inst.DisplayTerminationReason() + ")"
	}
	return "ended"
}

// formatTimelineOffset returns a "+Xm Ys" offset from a base timestamp.
func formatTimelineOffset(base, ts int64) string {
	d := time.Duration(ts-base) * time.Second
	if d < 0 {
		return fmt.Sprintf("-%s", (-d).Truncate(time.Second))
	}
	return "+" + d.Truncate(time.Second).String()
}

// formatPhaseDurations returns "setup 2m10s | run 45s | upload 12s" from timings.
func formatPhaseDurations(t *db.JobPhaseTimings) string {
	if t == nil {
		return ""
	}

	var parts []string
	dur := func(start, end *int64, label string) {
		if start != nil && end != nil && *end > *start {
			d := time.Duration(*end-*start) * time.Second
			parts = append(parts, fmt.Sprintf("%s %s", label, d.Truncate(time.Second)))
		}
	}
	dur(t.SetupStart, t.SetupEnd, "setup")
	dur(t.RunStart, t.RunEnd, "run")
	dur(t.UploadStart, t.UploadEnd, "upload")

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " | ")
}

func formatGPUMetrics(t *db.JobPhaseTimings) string {
	if t == nil {
		return ""
	}
	var parts []string
	if t.PeakGPUMemMiB != nil {
		parts = append(parts, fmt.Sprintf("peak mem %d MiB", *t.PeakGPUMemMiB))
	}
	if t.MeanGPUUtil != nil {
		parts = append(parts, fmt.Sprintf("mean util %d%%", *t.MeanGPUUtil))
	}
	if t.PeakGPUUtil != nil {
		parts = append(parts, fmt.Sprintf("peak util %d%%", *t.PeakGPUUtil))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

func formatInstanceDiagnoseReport(report *instanceDiagnoseReport) string {
	var b strings.Builder
	inst := report.Instance

	// Section 1: Header
	statusLabel := campaign.DisplayInstanceStatusWithReason(inst)
	fmt.Fprintf(&b, "Instance %s — %s — %s\n", ids.FormatInstanceID(inst.ID), displayInstanceGPU(inst), statusLabel)
	fmt.Fprintf(&b, "  Provider:  %s", inst.Provider)
	if pid := inst.EffectiveProviderID(); pid != "" {
		fmt.Fprintf(&b, " (%s)", pid)
	}
	b.WriteString("\n")
	if inst.DataCenter != "" {
		fmt.Fprintf(&b, "  Location:  %s\n", inst.DataCenter)
	}
	if report.Obs.Uptime != nil {
		fmt.Fprintf(&b, "  Uptime:    %s\n", report.Obs.Uptime.Truncate(time.Second))
	}
	if report.Obs.Cost != nil {
		costStr := fmt.Sprintf("$%.2f", *report.Obs.Cost)
		if report.Obs.Rate != nil {
			costStr += fmt.Sprintf("  ($%.2f/hr)", *report.Obs.Rate)
		}
		fmt.Fprintf(&b, "  Cost:      %s\n", costStr)
	}
	if report.LivePhase != "" || report.LivePhaseRaw != "" {
		b.WriteString("\nLive Phase:\n")
		if report.LivePhase != "" {
			label := campaign.InstancePhaseLabel(report.LivePhase)
			if report.LivePhaseChanged != nil {
				fmt.Fprintf(&b, "  Reconciled: %s (%s)\n", label, report.LivePhase)
				fmt.Fprintf(&b, "  Since:      %s\n", time.Since(*report.LivePhaseChanged).Truncate(time.Second))
			} else {
				fmt.Fprintf(&b, "  Reconciled: %s (%s)\n", label, report.LivePhase)
			}
		}
		if report.LivePhaseRaw != "" {
			fmt.Fprintf(&b, "  Raw R2:     %s (%s)\n", campaign.InstancePhaseLabel(report.LivePhaseRaw), report.LivePhaseRaw)
		}
	}

	// Section 2: Root cause
	b.WriteString("\n")
	fmt.Fprintf(&b, "Cause: %s\n", report.Diagnosis.Summary)

	// Section 3: Timeline
	if len(report.Timeline) > 0 {
		b.WriteString("\nTimeline:\n")
		base := timelineBase(inst)
		for _, entry := range report.Timeline {
			offset := formatTimelineOffset(base, entry.Timestamp)
			fmt.Fprintf(&b, "  %-12s %s\n", offset, entry.Label)
		}
	}

	// Section 4: Survival analysis
	if survSection := formatSurvivalSection(report); survSection != "" {
		b.WriteString("\n")
		b.WriteString(survSection)
	}

	// Section 5: Jobs
	if len(report.Jobs) > 0 {
		b.WriteString("\nJobs:\n")
		for _, j := range report.Jobs {
			outcome := report.Outcomes[j.ID]
			summary := summarizeJobIssue(j, outcome)

			statusPart := j.Status
			if outcome != "" && outcome != j.Status {
				statusPart = fmt.Sprintf("%s (attempt %s)", j.Status, outcome)
			}
			fmt.Fprintf(&b, "  Job #%d — %s\n", j.ID, statusPart)

			if summary != "" {
				fmt.Fprintf(&b, "    Diagnosis: %s\n", summary)
			}
			if desc := jobDescription(j); desc != "" {
				fmt.Fprintf(&b, "    Command:   %s\n", desc)
			}

			t := report.PhaseTimings[j.ID]
			if phases := formatPhaseDurations(t); phases != "" {
				fmt.Fprintf(&b, "    Phases:    %s\n", phases)
			} else if t == nil {
				fmt.Fprintf(&b, "    Phases:    (no phase data)\n")
			}
			if gpu := formatGPUMetrics(t); gpu != "" {
				fmt.Fprintf(&b, "    GPU:       %s\n", gpu)
			}
		}
	}

	return b.String()
}

func timelineBase(inst *db.Launch) int64 {
	if inst.LaunchedAt != nil {
		return *inst.LaunchedAt
	}
	return inst.CreatedAt
}

// formatSurvivalSection renders the survival analysis comparison.
func formatSurvivalSection(report *instanceDiagnoseReport) string {
	var b strings.Builder
	bs := report.BootstrapSurvival
	ss := report.SetupSurvival
	inst := report.Instance

	if bs == nil && ss == nil {
		return ""
	}

	b.WriteString("Survival Analysis:\n")

	if bs != nil {
		adaptive := bs.SampleSize >= 20
		thresholdType := "default"
		if adaptive {
			thresholdType = "adaptive"
		}
		fmt.Fprintf(&b, "  Bootstrap: n=%d, warn after %s, terminate after %s (%s)\n",
			bs.SampleSize,
			bs.WarnAfter.Truncate(time.Second),
			bs.TerminateAfter.Truncate(time.Second),
			thresholdType)

		// Compare this instance's bootstrap duration against thresholds.
		if inst.LaunchedAt != nil {
			var bootstrapEnd int64
			bootstrapLabel := ""

			// Check if any job ever started (wrapper_start).
			for _, t := range report.PhaseTimings {
				if t.WrapperStart != nil && (bootstrapEnd == 0 || *t.WrapperStart < bootstrapEnd) {
					bootstrapEnd = *t.WrapperStart
					bootstrapLabel = "bootstrapped"
				}
			}
			// If no job started and the instance failed, use ended_at.
			// For completed instances without phase data, skip — we can't determine when bootstrap finished.
			if bootstrapEnd == 0 && inst.EndedAt != nil && inst.Status == db.LaunchStatusFailed {
				bootstrapEnd = *inst.EndedAt
				bootstrapLabel = "terminated"
			}

			if bootstrapEnd > 0 {
				elapsed := time.Duration(bootstrapEnd-*inst.LaunchedAt) * time.Second
				comparison := compareThreshold(elapsed, bs.WarnAfter, bs.TerminateAfter)
				fmt.Fprintf(&b, "    This instance: %s at %s %s\n",
					bootstrapLabel, elapsed.Truncate(time.Second), comparison)
			}
		}
	}

	if ss != nil {
		adaptive := ss.SampleSize >= 20
		thresholdType := "default"
		if adaptive {
			thresholdType = "adaptive"
		}
		fmt.Fprintf(&b, "  Setup: n=%d, warn after %s, terminate after %s (%s)\n",
			ss.SampleSize,
			ss.WarnAfter.Truncate(time.Second),
			ss.TerminateAfter.Truncate(time.Second),
			thresholdType)

		// Find setup duration for this instance (iterate jobs for stable order).
		for _, j := range report.Jobs {
			t := report.PhaseTimings[j.ID]
			if t == nil || t.SetupStart == nil {
				continue
			}
			if t.SetupEnd != nil {
				elapsed := time.Duration(*t.SetupEnd-*t.SetupStart) * time.Second
				comparison := compareThreshold(elapsed, ss.WarnAfter, ss.TerminateAfter)
				fmt.Fprintf(&b, "    This instance: setup completed at %s %s\n",
					elapsed.Truncate(time.Second), comparison)
			} else if inst.EndedAt != nil {
				elapsed := time.Duration(*inst.EndedAt-*t.SetupStart) * time.Second
				comparison := compareThreshold(elapsed, ss.WarnAfter, ss.TerminateAfter)
				fmt.Fprintf(&b, "    This instance: setup stalled at %s %s\n",
					elapsed.Truncate(time.Second), comparison)
			}
			break
		}
	}

	return b.String()
}

// compareThreshold returns a comparison string like "(within warn threshold)"
// or "(exceeded terminate threshold by 2m)".
func compareThreshold(elapsed, warn, terminate time.Duration) string {
	if elapsed >= terminate {
		over := (elapsed - terminate).Truncate(time.Second)
		if over == 0 {
			return "(at terminate threshold)"
		}
		return fmt.Sprintf("(exceeded terminate threshold by %s)", over)
	}
	if elapsed >= warn {
		return fmt.Sprintf("(past warn threshold, %s until terminate)", (terminate - elapsed).Truncate(time.Second))
	}
	return "(within warn threshold)"
}

func jobDescription(j *db.Job) string {
	raw := j.Command
	if raw == "" {
		raw = j.Description
	}
	if raw == "" {
		return ""
	}
	// Multi-line shell commands (heredocs, `\` line continuations) must
	// render as a single line in the diagnose table.
	return campaign.TruncateCommand(strings.Join(strings.Fields(raw), " "), 80)
}
