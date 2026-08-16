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
	"github.com/osteele/weft/internal/r2upload"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

var instanceDiagnoseCmd = &cobra.Command{
	Use:   "diagnose <instance-id>",
	Short: "Deep diagnosis of a cloud instance with timeline and survival analysis",
	Long: `Shows a detailed diagnostic report for a single cloud instance, including:
- Root cause analysis
- Chronological timeline of all events (as offsets from launch)
- Bootstrap and setup survival analysis with learned/default threshold provenance
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
	BootstrapStage    string
	OnStartProbe      string // body of instance/<id>/onstart-probe — present iff OnStart actually ran
	OnStartStage      string // body of instance/<id>/onstart-stage — last successful OnStart step name
	Obs               terminal.CloudInstanceObservability
	Diagnosis         instanceDiagnosis
	Timeline          []timelineEntry
	// UploadFailures maps job ID → marker. Populated best-effort from R2;
	// missing entries either had a successful upload or no marker (e.g.
	// agent never reached the upload step).
	UploadFailures map[int64]r2upload.FailureMarker
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
		livePhase = reconcileInstanceDiagnoseLivePhase(jobs, liveState.InstancePhase)
		if liveState.PhaseChangedAt != nil {
			ts := time.Unix(*liveState.PhaseChangedAt, 0)
			livePhaseChanged = &ts
		}
	}
	if livePhase == "" {
		livePhase = reconcileInstanceDiagnoseLivePhase(jobs, "")
	}
	livePhaseRaw := ""
	bootstrapStage := ""
	onStartProbe := ""
	onStartStage := ""
	uploadFailures := map[int64]r2upload.FailureMarker{}
	if r2Client, err := newR2ClientFromConfig(); err == nil && r2Client != nil {
		fetch := func(key string) string {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if data, err := r2Client.GetObject(ctx, key); err == nil {
				return strings.TrimSpace(string(data))
			}
			return ""
		}
		livePhaseRaw = fetch(r2keys.InstancePhase(inst.ID))
		bootstrapStage = fetch(r2keys.BootstrapStage(inst.ID))
		onStartProbe = fetch(r2keys.InstanceOnStartProbe(inst.ID))
		onStartStage = fetch(r2keys.InstanceOnStartStage(inst.ID))

		// Upload-failure markers per job (best-effort; missing is normal).
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		for _, j := range jobs {
			runID := int64(0)
			if attempts, err := db.ListAttempts(database, j.ID); err == nil && len(attempts) > 0 {
				runID = attempts[0].ID
			}
			if m, ok := fetchJobUploadFailureMarker(ctx, r2Client, j.ID, runID); ok {
				uploadFailures[j.ID] = m
			}
		}
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
		BootstrapStage:    bootstrapStage,
		OnStartProbe:      onStartProbe,
		OnStartStage:      onStartStage,
		Obs:               obs,
		Diagnosis:         diagnosis,
		Timeline:          timeline,
		UploadFailures:    uploadFailures,
	}

	fmt.Print(formatInstanceDiagnoseReport(report))
	return nil
}

func reconcileInstanceDiagnoseLivePhase(jobs []*db.Job, phase string) string {
	phase = strings.TrimSpace(phase)
	if reconciled, _ := campaign.DisplayPhase(jobs, phase); reconciled != "" {
		return reconciled
	}
	return phase
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
		prefix := fmt.Sprintf("job %s", ids.FormatJobID(j.ID))
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

// formatBootstrapSection renders the bootstrap-stage block. The OnStart
// probe, OnStart stage, and bootstrap stage together disambiguate failure
// modes:
//   - bootstrap stage set         → bootstrap.sh ran and reached `stage`
//   - probe, onstart-stage, none  → OnStart's chain died at `onstart-stage`
//     (e.g. apt-installing, rclone-installing) — surfaces which install step failed
//   - probe, no stages            → OnStart ran but never wrote a stage marker
//   - no probe, no stages         → OnStart never executed, or no outbound network
//
// See docs/guides/cloud-instance-debugging.md.
func formatBootstrapSection(stage, probe, onstartStage string, launched bool) string {
	if stage != "" {
		out := fmt.Sprintf("\nBootstrap:\n  Last stage:    %s\n", stage)
		if probe != "" {
			out += fmt.Sprintf("  OnStart probe: %s\n", probe)
		}
		if onstartStage != "" {
			out += fmt.Sprintf("  OnStart stage: %s\n", onstartStage)
		}
		return out
	}
	if !launched {
		return ""
	}
	if probe != "" {
		out := "\nBootstrap:\n" +
			"  Last stage:    (none — bootstrap.sh did not write a marker)\n" +
			fmt.Sprintf("  OnStart probe: %s (OnStart ran; rclone setup or bootstrap.sh download failed)\n", probe)
		if onstartStage != "" {
			out += fmt.Sprintf("  OnStart stage: %s (last successful step before chain died)\n", onstartStage)
		}
		return out
	}
	return "\nBootstrap:\n" +
		"  Last stage:    (no marker on R2 — bootstrap.sh likely never ran)\n" +
		"  OnStart probe: (none — container probably never executed OnStart, or had no outbound network)\n"
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

	b.WriteString(formatBootstrapSection(report.BootstrapStage, report.OnStartProbe, report.OnStartStage, inst.LaunchedAt != nil))

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
			if uf, ok := report.UploadFailures[j.ID]; ok {
				fmt.Fprintf(&b, "    Upload:    truncated — %s (%s); %d/%d bytes in %.0fs\n",
					uf.Cause(), uf.Reason, uf.BytesUploaded, uf.BytesTotal, uf.ElapsedSeconds)
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
		fmt.Fprintf(&b, "  Bootstrap: n=%d, warn after %s (%s), terminate after %s (%s)\n",
			bs.SampleSize,
			bs.Warn.Duration().Truncate(time.Second),
			survivalThresholdProvenance(bs.Warn.IsLearned()),
			bs.Terminate.Duration().Truncate(time.Second),
			survivalThresholdProvenance(bs.Terminate.IsLearned()))

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
				comparison := compareThreshold(elapsed, bs.Warn.Duration(), bs.Terminate.Duration())
				fmt.Fprintf(&b, "    This instance: %s at %s %s\n",
					bootstrapLabel, elapsed.Truncate(time.Second), comparison)
			}
		}
	}

	if ss != nil {
		fmt.Fprintf(&b, "  Setup: n=%d, warn after %s (%s), terminate after %s (%s)\n",
			ss.SampleSize,
			ss.Warn.Duration().Truncate(time.Second),
			survivalThresholdProvenance(ss.Warn.IsLearned()),
			ss.Terminate.Duration().Truncate(time.Second),
			survivalThresholdProvenance(ss.Terminate.IsLearned()))

		// Find setup duration for this instance (iterate jobs for stable order).
		for _, j := range report.Jobs {
			t := report.PhaseTimings[j.ID]
			if t == nil || t.SetupStart == nil {
				continue
			}
			if t.SetupEnd != nil {
				elapsed := time.Duration(*t.SetupEnd-*t.SetupStart) * time.Second
				comparison := compareThreshold(elapsed, ss.Warn.Duration(), ss.Terminate.Duration())
				fmt.Fprintf(&b, "    This instance: setup completed at %s %s\n",
					elapsed.Truncate(time.Second), comparison)
			} else if inst.EndedAt != nil {
				elapsed := time.Duration(*inst.EndedAt-*t.SetupStart) * time.Second
				comparison := compareThreshold(elapsed, ss.Warn.Duration(), ss.Terminate.Duration())
				fmt.Fprintf(&b, "    This instance: setup stalled at %s %s\n",
					elapsed.Truncate(time.Second), comparison)
			}
			break
		}
	}

	return b.String()
}

func survivalThresholdProvenance(learned bool) string {
	if learned {
		return "learned"
	}
	return "default"
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
