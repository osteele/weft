package blockreason

import (
	"strings"
	"testing"
)

// probeThat returns a fixed structured breakdown for any launch reason, and
// records the launch reason it was asked to probe.
func probeThat(s *Structured) (ProbeFunc, *[]string) {
	var launches []string
	return func(launch string) *Structured {
		launches = append(launches, launch)
		return s
	}, &launches
}

func reuseRejection(instance, reason string) ReuseRejection {
	return ReuseRejection{Instance: instance, Reason: reason}
}

// probe returns a Structured with the given reuse rejections and the given
// summary. Launch is always set so the result reads as a placement failure.
func probeWithReuse(summary string, rejections ...ReuseRejection) *Structured {
	return &Structured{Summary: summary, Launch: "launch", Reuse: rejections}
}

func TestVerdictBuilderSettle_MaskingReuseDiagnosticNeverPrimary(t *testing.T) {
	// A code path that tries to overwrite the authoritative reason with a
	// reuse diagnostic must be ignored: the first authoritative reason wins,
	// and the diagnostic only ever trails it.
	diag := "could not reuse running instances: wi12 has 0 free GPU slots"
	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindMarket, "provider launch failed: connection reset")
	vb.SetLaunchBlocker(KindUnclassified, diag)
	vb.AddReuseDiagnostic(diag)

	flat, s := vb.Settle(nil)
	if !strings.HasPrefix(flat, "provider launch failed: connection reset") {
		t.Fatalf("flat = %q, want it to lead with the first authoritative launch reason", flat)
	}
	if strings.HasPrefix(flat, diag) {
		t.Fatalf("flat = %q, want the reuse diagnostic to never become primary", flat)
	}
	if s != nil {
		t.Fatalf("structured = %+v, want nil (no probe supplied)", s)
	}
}

func TestVerdictBuilderSettle_ReuseDiagnosticAppendsAsTrailingDetail(t *testing.T) {
	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindMarket, "no offers from providers for gpu=A40")
	vb.AddReuseDiagnostic("could not reuse running instances: wi12 has 0 free GPU slots")

	flat, _ := vb.Settle(nil)
	if !strings.HasPrefix(flat, "no offers from providers for gpu=A40") {
		t.Fatalf("flat = %q, want it to lead with the launch reason", flat)
	}
	if !strings.Contains(flat, "; could not reuse running instances") {
		t.Fatalf("flat = %q, want the reuse diagnostic appended after the launch reason", flat)
	}
}

func TestVerdictBuilderSettle_DeduplicatesReuseDiagnostics(t *testing.T) {
	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindBudget, "run-rate headroom exhausted ($0.24/hr free, this group needs $1.52/hr)")
	diag := "reuse blocked: wi2808 RTX 6000Ada 45GB: GPU class mismatch: job=h100 instance=NVIDIA"
	vb.AddReuseDiagnostic(diag)
	vb.AddReuseDiagnostic(diag)

	flat, _ := vb.Settle(nil)
	if strings.Count(flat, "reuse blocked") != 1 {
		t.Fatalf("flat = %q, want the duplicate reuse diagnostic recorded once", flat)
	}
}

func TestVerdictBuilderSettle_DiagOnlyBuilderSettlesEmpty(t *testing.T) {
	vb := &VerdictBuilder{}
	vb.AddReuseDiagnostic("could not reuse running instances: wi12 has 0 free GPU slots")

	flat, s := vb.Settle(nil)
	if flat != "" || s != nil {
		t.Fatalf("diag-only verdict settled to (%q, %+v), want empty", flat, s)
	}
}

func TestVerdictBuilderSettle_NoRentalHeadroomGetsProbedStructured(t *testing.T) {
	// Rule b: a no-rental-headroom blocker without a structured form gets one
	// probed when the probe is a placement failure.
	probe, launches := probeThat(probeWithReuse("no rental headroom; running instances couldn't accept this job",
		reuseRejection("wi12", "GPU class mismatch")))
	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindBudget, "no rental headroom; running instances couldn't accept this job")

	flat, s := vb.Settle(probe)
	if flat != "no rental headroom; running instances couldn't accept this job" {
		t.Fatalf("flat = %q, want unchanged", flat)
	}
	if s == nil || !s.IsPlacementFailure() {
		t.Fatalf("structured = %+v, want a probed placement failure", s)
	}
	if len(*launches) != 1 || (*launches)[0] != NoRentalHeadroomHeadline {
		t.Fatalf("probe launches = %v, want [%q]", *launches, NoRentalHeadroomHeadline)
	}
}

func TestVerdictBuilderSettle_BudgetBlockerGetsProbedStructuredOnlyWithReuseRejections(t *testing.T) {
	// Rule c: a run-rate budget blocker gets a probed breakdown only when
	// probing yields at least one reuse rejection.
	runRate := "run-rate target exceeded: target $2.00/hr, current $1.80/hr + requested $0.50/hr = $2.30/hr (headroom $0.20/hr, job needs $0.50/hr)"

	t.Run("reuse rejections recovered", func(t *testing.T) {
		probe, _ := probeThat(probeWithReuse(runRate, reuseRejection("wi12", "GPU memory insufficient")))
		vb := &VerdictBuilder{}
		vb.SetLaunchBlocker(KindBudget, runRate)
		_, s := vb.Settle(probe)
		if s == nil || len(s.Reuse) == 0 {
			t.Fatalf("structured = %+v, want probed reuse rejections", s)
		}
	})

	t.Run("busy-but-compatible instance yields no structured form", func(t *testing.T) {
		// The probe found a compatible instance, so Reuse is empty and the
		// budget reason stays the whole story.
		probe, _ := probeThat(&Structured{Summary: runRate, Launch: runRate})
		vb := &VerdictBuilder{}
		vb.SetLaunchBlocker(KindBudget, runRate)
		_, s := vb.Settle(probe)
		if s != nil {
			t.Fatalf("structured = %+v, want nil (budget-only blocker)", s)
		}
	})
}

func TestVerdictBuilderSettle_ProbedLaunchStripsReuseDiagnostics(t *testing.T) {
	// A budget blocker that already carries a trailing reuse diagnostic must
	// probe with the clean launch portion, not the composite string.
	runRate := "run-rate headroom exhausted ($1.60/hr free, this group needs $1.65/hr)"
	composite := runRate + "; could not reuse running instances: wi5212 A100 PCIE 40GB: image incompatible"
	probe, launches := probeThat(probeWithReuse(runRate, reuseRejection("wi5212", "GPU memory insufficient")))

	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindBudget, runRate)
	vb.AddReuseDiagnostic("could not reuse running instances: wi5212 A100 PCIE 40GB: image incompatible")

	flat, s := vb.Settle(probe)
	if flat != composite {
		t.Fatalf("flat = %q, want %q", flat, composite)
	}
	if s == nil || len(s.Reuse) == 0 {
		t.Fatalf("structured = %+v, want probed reuse rejections", s)
	}
	if (*launches)[0] != runRate {
		t.Fatalf("probe launch = %q, want the clean budget reason %q", (*launches)[0], runRate)
	}
}

func TestVerdictBuilderSettle_RelaunchPathBlockerGetsSummaryFlat(t *testing.T) {
	// Rule d: a blocker with observed reuse detail gets a probed structured
	// form whose Summary is the flat reason.
	launchReason := "no offers available"
	diag := "could not reuse running instances: wi12 CPU cores insufficient"
	flatWant := launchReason + "; " + diag
	probe, _ := probeThat(probeWithReuse(launchReason, reuseRejection("wi12", "CPU cores insufficient")))

	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindUnclassified, launchReason)
	vb.AddReuseDiagnostic(diag)

	flat, s := vb.Settle(probe)
	if flat != flatWant {
		t.Fatalf("flat = %q, want %q", flat, flatWant)
	}
	if s == nil {
		t.Fatal("structured = nil, want a probed form")
	}
	if s.Summary != flatWant {
		t.Fatalf("structured.Summary = %q, want the flat reason %q", s.Summary, flatWant)
	}
}

func TestVerdictBuilderSettle_RecordedReuseOverlaysProbeAndOnPremRidesAlong(t *testing.T) {
	// Rule e: recorded reuse failures overlay re-probed entries for the same
	// instance, and on-prem detail rides along on Structured.OnPrem.
	probed := &Structured{
		Summary: "provider launch failed: test outage",
		Launch:  "provider launch failed: test outage",
		Reuse:   []ReuseRejection{reuseRejection("wi12", "GPU memory insufficient")},
	}
	probe, _ := probeThat(probed)
	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindMarket, "provider launch failed: test outage")
	vb.RecordReuse([]ReuseRejection{{Instance: "wi12", Reason: "submit failed: upload source: connection reset", Detail: "full error text"}})
	vb.SetOnPremDetail("cool30: driver floor: NVIDIA driver 525.125.06 < required >=570")

	_, s := vb.Settle(probe)
	if s == nil {
		t.Fatal("structured = nil")
	}
	if len(s.Reuse) != 1 || s.Reuse[0].Reason != "submit failed: upload source: connection reset" {
		t.Fatalf("reuse = %+v, want the recorded rejection to overlay the probe", s.Reuse)
	}
	if s.OnPrem != "cool30: driver floor: NVIDIA driver 525.125.06 < required >=570" {
		t.Fatalf("OnPrem = %q, want the recorded on-prem detail", s.OnPrem)
	}
}

func TestVerdictBuilderSettle_PlannerStructuredSkipsProbeButMergesRecordedReuse(t *testing.T) {
	planner := &Structured{
		Summary: "planner: no compatible offers",
		Launch:  "planner: no compatible offers",
		Reuse:   []ReuseRejection{reuseRejection("wi12", "GPU memory insufficient")},
	}
	probe, launches := probeThat(probeWithReuse("planner: no compatible offers"))
	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindMarket, "planner: no compatible offers")
	vb.SetPlannerStructured(planner)
	vb.RecordReuse([]ReuseRejection{{Instance: "wi12", Reason: "submit failed: source too large"}})
	vb.SetOnPremDetail("cool30: reject")

	flat, s := vb.Settle(probe)
	if flat != "planner: no compatible offers" {
		t.Fatalf("flat = %q, want unchanged", flat)
	}
	if s != planner {
		t.Fatalf("structured = %+v, want the planner's form used as-is", s)
	}
	if len(*launches) != 0 {
		t.Fatalf("probe called %d times, want 0 (planner form skips probing)", len(*launches))
	}
	if len(s.Reuse) != 1 || s.Reuse[0].Reason != "submit failed: source too large" {
		t.Fatalf("reuse = %+v, want the recorded rejection to overlay the planner form", s.Reuse)
	}
	if s.OnPrem != "cool30: reject" {
		t.Fatalf("OnPrem = %q, want the on-prem detail", s.OnPrem)
	}
}

func TestBlockerKindRecheckNeed(t *testing.T) {
	tests := []struct {
		kind  BlockerKind
		want  RecheckNeed
		typed bool
	}{
		{kind: KindMarket, want: RecheckMarket, typed: true},
		{kind: KindBudget, want: RecheckNone, typed: true},
		{kind: KindBackoff, want: RecheckDeadline, typed: true},
		{kind: KindPrecondition, want: RecheckNone, typed: true},
		{kind: KindUnclassified, want: RecheckNone, typed: false},
	}
	for _, tc := range tests {
		t.Run(tc.kind.String(), func(t *testing.T) {
			if got, typed := tc.kind.RecheckNeed(); got != tc.want || typed != tc.typed {
				t.Errorf("RecheckNeed(%v) = (%v, %v), want (%v, %v)", tc.kind, got, typed, tc.want, tc.typed)
			}
		})
	}
}

func TestVerdictBuilderNeed_UnclassifiedFallsBackToStringRecheck(t *testing.T) {
	tests := []struct {
		name string
		vb   func() *VerdictBuilder
		want RecheckNeed
	}{
		{
			name: "backoff countdown classifies as deadline",
			vb: func() *VerdictBuilder {
				vb := &VerdictBuilder{}
				vb.SetLaunchBlocker(KindUnclassified, "retry backoff 1m30s remaining (after 3 failure(s))")
				return vb
			},
			want: RecheckDeadline,
		},
		{
			name: "market string keeps the timer",
			vb: func() *VerdictBuilder {
				vb := &VerdictBuilder{}
				vb.SetLaunchBlocker(KindUnclassified, "no offers available")
				return vb
			},
			want: RecheckMarket,
		},
		{
			name: "unrecognized string keeps the timer",
			vb: func() *VerdictBuilder {
				vb := &VerdictBuilder{}
				vb.SetLaunchBlocker(KindUnclassified, "something nobody classified yet")
				return vb
			},
			want: RecheckMarket,
		},
		{
			name: "typed budget ignores the string",
			vb: func() *VerdictBuilder {
				vb := &VerdictBuilder{}
				vb.SetLaunchBlocker(KindBudget, "run-rate headroom exhausted ($0.24/hr free, this group needs $1.52/hr)")
				return vb
			},
			want: RecheckNone,
		},
		{
			name: "typed backoff ignores the string",
			vb: func() *VerdictBuilder {
				vb := &VerdictBuilder{}
				vb.SetLaunchBlocker(KindBackoff, "reuse backoff 45s remaining (after 2 failed submit(s))")
				return vb
			},
			want: RecheckDeadline,
		},
		{
			// The string path would misclassify a dependency reason as market;
			// the typed kind wins.
			name: "typed precondition overrides the string misclassification",
			vb: func() *VerdictBuilder {
				vb := &VerdictBuilder{}
				vb.SetLaunchBlocker(KindPrecondition, "waiting on job dependency: wj1 must complete")
				return vb
			},
			want: RecheckNone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vb().Need(); got != tc.want {
				t.Errorf("Need() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVerdictBuilderNeed_BackoffCountdownInDiagnosticsRaisesDeadline(t *testing.T) {
	// A backoff countdown nested inside a reuse diagnostic names a clock
	// gating the reuse avenue; a database-observable primary must not
	// suppress the timer that discovers the deadline passing (the same
	// whole-reason rule Recheck applies to strings).
	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindBudget, "run-rate target exceeded")
	vb.AddReuseDiagnostic("could not reuse running instances: reuse backoff 45s remaining (after 2 failure(s))")
	if got := vb.Need(); got != RecheckDeadline {
		t.Errorf("Need() = %v, want RecheckDeadline", got)
	}

	// A market primary already outranks the deadline; the countdown must not
	// lower it.
	vb = &VerdictBuilder{}
	vb.SetLaunchBlocker(KindMarket, "no offers available")
	vb.AddReuseDiagnostic("could not reuse running instances: reuse backoff 45s remaining (after 2 failure(s))")
	if got := vb.Need(); got != RecheckMarket {
		t.Errorf("Need() = %v, want RecheckMarket", got)
	}

	// Without a countdown the budget primary keeps its quiet class.
	vb = &VerdictBuilder{}
	vb.SetLaunchBlocker(KindBudget, "run-rate target exceeded")
	vb.AddReuseDiagnostic("could not reuse running instances: gpu class mismatch")
	if got := vb.Need(); got != RecheckNone {
		t.Errorf("Need() = %v, want RecheckNone", got)
	}
}

func TestVerdictBuilderAddBlocker_JoinsOntoPrimary(t *testing.T) {
	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindMarket, "no compatible offers")
	vb.AddBlocker(KindPrecondition, "waiting for wj7 to succeed (status: running)")
	flat, _ := vb.Settle(nil)
	if want := "no compatible offers; waiting for wj7 to succeed (status: running)"; flat != want {
		t.Errorf("flat = %q, want %q", flat, want)
	}
	// The market primary sets the cadence; the precondition fragment rides
	// along without lowering it.
	if got := vb.Need(); got != RecheckMarket {
		t.Errorf("Need() = %v, want RecheckMarket", got)
	}
	// A contained fragment is not appended twice.
	vb.AddBlocker(KindPrecondition, "waiting for wj7 to succeed (status: running)")
	flat, _ = vb.Settle(nil)
	if want := "no compatible offers; waiting for wj7 to succeed (status: running)"; flat != want {
		t.Errorf("flat after duplicate = %q, want %q", flat, want)
	}
}

func TestVerdictBuilderAddBlocker_FirstBlockerBecomesPrimary(t *testing.T) {
	vb := &VerdictBuilder{}
	vb.AddBlocker(KindPrecondition, "waiting for wj7 to succeed (status: running)")
	flat, _ := vb.Settle(nil)
	if want := "waiting for wj7 to succeed (status: running)"; flat != want {
		t.Errorf("flat = %q, want %q", flat, want)
	}
	if got := vb.Need(); got != RecheckNone {
		t.Errorf("Need() = %v, want RecheckNone", got)
	}
}

func TestVerdictBuilderReplaceLaunchBlocker_SupersedesPriorVerdict(t *testing.T) {
	vb := &VerdictBuilder{}
	vb.SetLaunchBlocker(KindBudget, "run-rate target exceeded")
	vb.AddBlocker(KindPrecondition, "waiting for wj7 to succeed (status: running)")
	vb.ReplaceLaunchBlocker(KindMarket, "no offers matched after retry")
	flat, _ := vb.Settle(nil)
	if want := "no offers matched after retry"; flat != want {
		t.Errorf("flat = %q, want %q", flat, want)
	}
	if got := vb.Need(); got != RecheckMarket {
		t.Errorf("Need() = %v, want RecheckMarket", got)
	}
}
