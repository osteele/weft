package edge

import (
	"strings"
	"testing"
	"time"
)

func mins(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, v := range n {
		out[i] = time.Duration(v) * time.Minute
	}
	return out
}

// Each regime must label what kind of claim it is making. The n=0 case is the
// one most likely to be printed as though it were evidence.
func TestRegimesLabelTheirOwnEvidence(t *testing.T) {
	cases := []struct {
		name    string
		samples []time.Duration
		regime  Regime
		must    []string
		mustNot []string
	}{
		{
			name:    "no samples states it is a declaration",
			samples: nil,
			regime:  RegimeDeclared,
			must:    []string{"declared envelope", "not a measurement"},
			mustNot: []string{"p50", "p90", "observed"},
		},
		{
			name:    "few samples reports a range with its count",
			samples: mins(4, 6, 9),
			regime:  RegimeProvisional,
			must:    []string{"observed", "3 run(s)", "provisional"},
			mustNot: []string{"p50", "p90"},
		},
		{
			name:    "enough samples reports percentiles",
			samples: mins(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12),
			regime:  RegimeMeasured,
			must:    []string{"p50", "p90", "12 runs"},
			mustNot: []string{"provisional", "declared"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewExpectation("job", 5*time.Minute, 2*time.Hour, 6*time.Hour, tc.samples)
			if e.Regime != tc.regime {
				t.Fatalf("regime = %s, want %s", e.Regime, tc.regime)
			}
			line := e.StatisticsLine()
			for _, want := range tc.must {
				if !strings.Contains(line, want) {
					t.Errorf("statistics line %q missing %q", line, want)
				}
			}
			for _, bad := range tc.mustNot {
				if strings.Contains(line, bad) {
					t.Errorf("statistics line %q must not claim %q", line, bad)
				}
			}
		})
	}
}

// Four samples must never be dressed up as a distribution.
func TestProvisionalNeverPrintsPercentiles(t *testing.T) {
	e := NewExpectation("job", time.Minute, time.Hour, 6*time.Hour, mins(3, 4, 5, 6))
	if strings.Contains(e.StatisticsLine(), "p50") {
		t.Fatal("four samples were reported as a distribution")
	}
	if !strings.Contains(e.StatisticsLine(), "4 run(s)") {
		t.Error("provisional statistics must carry their sample count")
	}
}

// The escalation trigger is a configured constant, identical in every regime,
// so agent behavior does not depend on how much history exists.
func TestEscalationTriggerIsConstantAcrossRegimes(t *testing.T) {
	escalate := 90 * time.Minute
	for _, samples := range [][]time.Duration{nil, mins(1, 2), mins(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11)} {
		e := NewExpectation("job", time.Minute, escalate, 6*time.Hour, samples)
		if v := e.Judge(escalate-time.Minute, 0); v.Escalate {
			t.Errorf("regime %s escalated before the constant", e.Regime)
		}
		if v := e.Judge(escalate, 0); !v.Escalate {
			t.Errorf("regime %s did not escalate at the constant", e.Regime)
		}
	}
}

// The stall trigger catches a wedged submission the elapsed ceiling would not
// reach for a long time yet.
func TestStallTriggerFiresBeforeTheCeiling(t *testing.T) {
	e := NewExpectation("job", time.Minute, 48*time.Hour, 6*time.Hour, nil)
	v := e.Judge(7*time.Hour, 7*time.Hour)
	if !v.Escalate {
		t.Fatal("a phase stuck for seven hours did not escalate")
	}
	if !strings.Contains(v.Text, "stall") {
		t.Errorf("stall escalation should name the stall trigger: %q", v.Text)
	}
}

// Every progress line carries all five required elements, in every regime.
func TestProgressCarriesAllFiveElements(t *testing.T) {
	for _, samples := range [][]time.Duration{nil, mins(2, 3), mins(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)} {
		e := NewExpectation("job", 5*time.Minute, 2*time.Hour, 6*time.Hour, samples)
		line := e.Progress(30*time.Minute, 10*time.Minute, "verifying")
		for _, want := range []string{"elapsed", "phase verifying", "escalate at"} {
			if !strings.Contains(line, want) {
				t.Errorf("regime %s progress missing %q: %s", e.Regime, want, line)
			}
		}
		if !strings.Contains(line, "within expectation") && !strings.Contains(line, "ESCALATE") {
			t.Errorf("regime %s progress carries no verdict: %s", e.Regime, line)
		}
	}
}

// The verdict must actually flip. A wait that can only ever reassure is worse
// than one that prints nothing.
func TestVerdictFlipsPastTheTrigger(t *testing.T) {
	e := NewExpectation("job", 5*time.Minute, time.Hour, 6*time.Hour, nil)
	if v := e.Judge(30*time.Minute, time.Minute); v.Escalate {
		t.Error("escalated inside the expected window")
	} else if !strings.Contains(v.Text, "no action indicated") {
		t.Errorf("normal verdict should settle the reader: %q", v.Text)
	}
	if v := e.Judge(15*time.Hour, time.Minute); !v.Escalate {
		t.Fatal("a fifteen-hour wait still reported as normal")
	}
}

// Elapsed must be measurable from the submission itself, or a resumed wait
// restarts its clock and an hours-long escalation ceiling is unreachable.
func TestNonceTimeRecoversTheSubmissionTime(t *testing.T) {
	want := time.UnixMilli(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC).UnixMilli()).UTC()
	nonce, err := NewNonce(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NonceTime(nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatalf("NonceTime = %s, want %s", got, want)
	}
}

func TestNonceTimeRejectsMalformedNonces(t *testing.T) {
	for _, bad := range []string{"", "short", strings.Repeat("U", 26)} {
		if _, err := NonceTime(bad); err == nil {
			t.Errorf("NonceTime accepted %q", bad)
		}
	}
}
