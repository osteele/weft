package credit

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// Tests use a fixed reference time so failures are deterministic and easy
// to read. T0 is the burst peak in most scenarios.
var T0 = time.Date(2026, 6, 1, 0, 24, 0, 0, time.UTC)

func failed(id int64, provider string, endedAt time.Time) *db.Launch {
	t := endedAt.Unix()
	return &db.Launch{
		ID:                id,
		Provider:          provider,
		Status:            db.LaunchStatusFailed,
		EndedAt:           &t,
		TerminationReason: db.TerminationReasonInfraFailure,
	}
}

func running(id int64, provider string, runningAt time.Time) *db.Launch {
	t := runningAt.Unix()
	return &db.Launch{
		ID:                id,
		Provider:          provider,
		ProviderRunningAt: &t,
	}
}

// runningEnded mirrors running but also sets EndedAt — used by tests
// that exercise the survivor check, which needs to know whether a
// launch outlived the burst window.
func runningEnded(id int64, provider string, runningAt, endedAt time.Time) *db.Launch {
	l := running(id, provider, runningAt)
	t := endedAt.Unix()
	l.EndedAt = &t
	return l
}

func memberIDs(inc *Incident) []int64 {
	ids := make([]int64, len(inc.Members))
	for i, m := range inc.Members {
		ids[i] = m.ID
	}
	return ids
}

func eqInt64Slice(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDetect_CleanIncident(t *testing.T) {
	// 5 destroys in 6 minutes on vastai, then 5 minutes of silence, then a
	// recovery launch reaches running. Classic wallet-zero pattern.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-3*time.Minute)),
		failed(4, "vastai", T0.Add(-3*time.Minute)),
		failed(5, "vastai", T0.Add(-6*time.Minute)),
	}
	runs := []*db.Launch{
		// Pre-incident launch that finished well before the burst started —
		// not a survivor, so it doesn't disqualify the burst.
		runningEnded(100, "vastai", T0.Add(-2*time.Hour), T0.Add(-30*time.Minute)),
		running(200, "vastai", T0.Add(5*time.Minute)), // recovery
	}
	res := Detect(Config{}, failures, runs, T0.Add(10*time.Minute))
	if res.Incident == nil {
		t.Fatalf("expected incident, got nil; rejections=%+v", res.Rejections)
	}
	if res.Incident.Provider != "vastai" {
		t.Errorf("Provider = %q, want vastai", res.Incident.Provider)
	}
	if res.Incident.BurstCount != 5 {
		t.Errorf("BurstCount = %d, want 5", res.Incident.BurstCount)
	}
	if !res.Incident.BurstEnd.Equal(T0) {
		t.Errorf("BurstEnd = %v, want %v", res.Incident.BurstEnd, T0)
	}
	if got, want := res.Incident.Silence, 5*time.Minute; got != want {
		t.Errorf("Silence = %v, want %v", got, want)
	}
	if res.Incident.Ongoing {
		t.Error("Ongoing = true, want false")
	}
	// Tiebreak: with id 3 and id 4 both at T0-3m, newer ID wins.
	if got := memberIDs(res.Incident); !eqInt64Slice(got, []int64{1, 2, 4, 3, 5}) {
		t.Errorf("Members = %v, want [1 2 4 3 5]", got)
	}
}

func TestDetect_RegionalOutageRejected(t *testing.T) {
	// 4 destroys on vastai, but another vastai instance reaches running
	// 90 seconds into the supposed "silence". That's not credit exhaustion
	// — that's a regional/SKU outage.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-2*time.Minute)),
		failed(4, "vastai", T0.Add(-3*time.Minute)),
	}
	runs := []*db.Launch{
		running(50, "vastai", T0.Add(-1*time.Minute)), // mid-incident: disqualifying
		running(60, "vastai", T0.Add(10*time.Minute)),
	}
	res := Detect(Config{}, failures, runs, T0.Add(20*time.Minute))
	if res.Incident != nil {
		t.Fatalf("expected nil incident (regional outage), got %+v", res.Incident)
	}
	if len(res.Rejections) == 0 {
		t.Fatal("expected at least one rejection diagnostic")
	}
	if res.Rejections[0].Reason != "regional_outage" {
		t.Errorf("rejection reason = %q, want regional_outage", res.Rejections[0].Reason)
	}
}

func TestDetect_ShortSilenceRejected(t *testing.T) {
	// 4 destroys, recovery 30s later — below the 90s MinSilence floor.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-2*time.Minute)),
		failed(4, "vastai", T0.Add(-3*time.Minute)),
	}
	runs := []*db.Launch{
		running(50, "vastai", T0.Add(30*time.Second)),
	}
	res := Detect(Config{}, failures, runs, T0.Add(20*time.Minute))
	if res.Incident != nil {
		t.Fatalf("expected nil incident (short silence), got %+v", res.Incident)
	}
	if len(res.Rejections) == 0 || res.Rejections[0].Reason != "short_silence" {
		t.Fatalf("expected short_silence rejection, got %+v", res.Rejections)
	}
}

func TestDetect_BelowBurstThreshold(t *testing.T) {
	// Only 2 destroys — below BurstMin (default 3).
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
	}
	res := Detect(Config{}, failures, nil, T0.Add(20*time.Minute))
	if res.Incident != nil {
		t.Fatalf("expected nil incident, got %+v", res.Incident)
	}
	if len(res.Rejections) != 0 {
		t.Errorf("expected no rejections (sub-threshold isn't a candidate), got %+v", res.Rejections)
	}
}

func TestDetect_StragglersViaLeftPad(t *testing.T) {
	// Burst of 3 within 5 min, plus an older failure 12 min back. With
	// default LeftPad=15m, the older failure joins the cluster.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-2*time.Minute)),
		failed(3, "vastai", T0.Add(-4*time.Minute)),
		failed(4, "vastai", T0.Add(-12*time.Minute)),
	}
	runs := []*db.Launch{
		running(50, "vastai", T0.Add(10*time.Minute)),
	}
	res := Detect(Config{}, failures, runs, T0.Add(30*time.Minute))
	if res.Incident == nil {
		t.Fatalf("expected incident, got nil; rejections=%+v", res.Rejections)
	}
	if res.Incident.BurstCount != 3 {
		t.Errorf("BurstCount = %d, want 3", res.Incident.BurstCount)
	}
	if got := memberIDs(res.Incident); !eqInt64Slice(got, []int64{1, 2, 3, 4}) {
		t.Errorf("Members = %v, want [1 2 3 4] (LeftPad should pull in id=4)", got)
	}
}

func TestDetect_OngoingIncident(t *testing.T) {
	// Burst happened, but no recovery yet. As long as silence >= MinSilence,
	// the incident is detected as Ongoing.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-3*time.Minute)),
	}
	now := T0.Add(20 * time.Minute)
	res := Detect(Config{}, failures, nil, now)
	if res.Incident == nil {
		t.Fatalf("expected ongoing incident, got nil; rejections=%+v", res.Rejections)
	}
	if !res.Incident.Ongoing {
		t.Error("Ongoing = false, want true")
	}
	if res.Incident.RecoveryLaunch != nil {
		t.Error("RecoveryLaunch should be nil for ongoing incident")
	}
	if !res.Incident.Recovery.Equal(now) {
		t.Errorf("Recovery = %v, want now=%v", res.Incident.Recovery, now)
	}
}

func TestDetect_OngoingBurstStillInProgress(t *testing.T) {
	// Burst peak was just 30s ago — below MinSilence. Should not yet
	// declare an incident; operator should rerun after the silence floor.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-2*time.Minute)),
	}
	res := Detect(Config{}, failures, nil, T0.Add(30*time.Second))
	if res.Incident != nil {
		t.Fatalf("expected nil (burst in progress), got %+v", res.Incident)
	}
}

func TestDetect_ProviderIsolation(t *testing.T) {
	// Same wall-clock burst, but split across two providers (2 vastai + 2
	// runpod). Neither provider alone hits BurstMin=3, so nothing is
	// detected. Cross-provider mixing must NOT happen.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "runpod", T0.Add(-2*time.Minute)),
		failed(4, "runpod", T0.Add(-3*time.Minute)),
	}
	res := Detect(Config{}, failures, nil, T0.Add(30*time.Minute))
	if res.Incident != nil {
		t.Fatalf("expected nil (sub-threshold per provider), got %+v", res.Incident)
	}
}

func TestDetect_CrossProviderRecoveryIgnored(t *testing.T) {
	// Vastai burst, but the only "recovery" signal is on runpod. The vastai
	// incident should be reported as Ongoing because runpod's success is
	// irrelevant to vastai's wallet.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-3*time.Minute)),
	}
	runs := []*db.Launch{
		running(100, "runpod", T0.Add(2*time.Minute)), // not vastai — must be ignored
	}
	res := Detect(Config{}, failures, runs, T0.Add(30*time.Minute))
	if res.Incident == nil {
		t.Fatalf("expected vastai incident, got nil; rejections=%+v", res.Rejections)
	}
	if !res.Incident.Ongoing {
		t.Error("Ongoing = false; runpod recovery must not satisfy vastai silence")
	}
	if res.Incident.Provider != "vastai" {
		t.Errorf("Provider = %q, want vastai", res.Incident.Provider)
	}
}

func TestDetect_CrossProviderRunningDoesNotAbort(t *testing.T) {
	// Vastai burst with a runpod instance reaching running mid-window.
	// This must NOT trigger the regional_outage rejection — different
	// provider, different wallet.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-3*time.Minute)),
	}
	runs := []*db.Launch{
		running(100, "runpod", T0.Add(-1*time.Minute)), // mid-window on different provider
		running(200, "vastai", T0.Add(10*time.Minute)), // legitimate vastai recovery
	}
	res := Detect(Config{}, failures, runs, T0.Add(30*time.Minute))
	if res.Incident == nil {
		t.Fatalf("expected incident (runpod running must not abort), got nil; rejections=%+v", res.Rejections)
	}
	for _, r := range res.Rejections {
		if r.Reason == "regional_outage" {
			t.Errorf("got cross-provider regional_outage rejection: %+v", r)
		}
	}
}

func TestDetect_PicksMostRecentAcrossProviders(t *testing.T) {
	// Two real incidents: older on vastai, newer on runpod. Should pick the
	// newer one but not lose the vastai incident from a future detect run.
	older := T0.Add(-3 * time.Hour)
	failures := []*db.Launch{
		// Newer burst on runpod
		failed(10, "runpod", T0),
		failed(11, "runpod", T0.Add(-1*time.Minute)),
		failed(12, "runpod", T0.Add(-3*time.Minute)),
		// Older burst on vastai
		failed(20, "vastai", older),
		failed(21, "vastai", older.Add(-1*time.Minute)),
		failed(22, "vastai", older.Add(-3*time.Minute)),
	}
	runs := []*db.Launch{
		running(100, "runpod", T0.Add(10*time.Minute)),
		running(200, "vastai", older.Add(10*time.Minute)),
	}
	res := Detect(Config{}, failures, runs, T0.Add(30*time.Minute))
	if res.Incident == nil {
		t.Fatal("expected an incident")
	}
	if res.Incident.Provider != "runpod" {
		t.Errorf("Provider = %q, want runpod (most recent)", res.Incident.Provider)
	}
}

func TestDetect_EmptyInputs(t *testing.T) {
	res := Detect(Config{}, nil, nil, T0)
	if res == nil || res.Incident != nil || len(res.Rejections) != 0 {
		t.Errorf("unexpected result on empty input: %+v", res)
	}
}

func TestDetect_LaunchesMissingTimestampsAreSkipped(t *testing.T) {
	// A launch with EndedAt == nil should not crash and should not count
	// toward the burst.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		{ID: 3, Provider: "vastai", Status: db.LaunchStatusFailed}, // no EndedAt
	}
	res := Detect(Config{}, failures, nil, T0.Add(30*time.Minute))
	if res.Incident != nil {
		// 2 valid timestamps is below BurstMin=3.
		t.Errorf("expected nil incident (only 2 valid timestamps), got %+v", res.Incident)
	}
}

// TestDetect_BurstVictimsDoNotSelfReject regression-tests the bug where a
// credit victim that briefly reached running before Vast destroyed it
// would appear in both the failures list and the running-signals list,
// then self-trigger the regional_outage rejection. DetectFromDB filters
// burst-victim IDs out of the running list before invoking Detect; this
// test simulates that filter by leaving the victim's ProviderRunningAt
// out of the runs slice while still keeping the victim in failures.
//
// To catch the bug at the Detect level (regardless of DetectFromDB), the
// detector itself should ignore running signals that share an ID with a
// burst member. Until that defense-in-depth is added, this test pins the
// expected behavior post-filter.
func TestDetect_BurstVictimsDoNotSelfReject(t *testing.T) {
	// 4 vastai destroys. Imagine wi2 had reached running 60s before being
	// killed by the wallet event — its provider_running_at falls inside
	// [burstStart, recoveryTS) and would falsely look like a regional
	// outage. Filtered out, the incident should be detected cleanly.
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-2*time.Minute)),
		failed(4, "vastai", T0.Add(-3*time.Minute)),
	}
	// Only the legitimate recovery is in the trusted runs list.
	runs := []*db.Launch{
		running(50, "vastai", T0.Add(5*time.Minute)),
	}
	res := Detect(Config{}, failures, runs, T0.Add(20*time.Minute))
	if res.Incident == nil {
		t.Fatalf("expected incident, got nil; rejections=%+v", res.Rejections)
	}
	if res.Incident.BurstCount != 4 {
		t.Errorf("BurstCount = %d, want 4", res.Incident.BurstCount)
	}
	for _, r := range res.Rejections {
		if r.Reason == "regional_outage" {
			t.Errorf("burst victim must not trigger regional_outage: %+v", r)
		}
	}
}

// TestDetect_SurvivorThroughBurstRejected pins the regression for the
// real 2026-05-31 23:43–23:54 cluster on vastai. Three H100/A100
// instances were destroyed within ~1m, but another vastai launch (a
// successful job) was already running through that window — proving
// the wallet was funded throughout. Without the survivor check, the
// detector reported a credit-exhaustion incident; with it, the burst
// is rejected as a regional outage.
func TestDetect_SurvivorThroughBurstRejected(t *testing.T) {
	failures := []*db.Launch{
		failed(1, "vastai", T0), // burst peak
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-3*time.Minute)),
		failed(4, "vastai", T0.Add(-12*time.Minute)), // straggler in LeftPad
	}
	// Survivor: started running well before the burst, completed past
	// the burst end. Vast couldn't have killed this if the wallet were
	// empty.
	survivor := runningEnded(99, "vastai",
		T0.Add(-15*time.Minute), // running well before burstStart
		T0.Add(2*time.Minute),   // ended after burstEnd
	)
	recovery := running(100, "vastai", T0.Add(10*time.Minute))
	res := Detect(Config{}, failures, []*db.Launch{survivor, recovery}, T0.Add(30*time.Minute))
	if res.Incident != nil {
		t.Fatalf("expected nil incident (survivor disproves credit theory), got %+v", res.Incident)
	}
	if len(res.Rejections) == 0 || res.Rejections[0].Reason != "regional_outage" {
		t.Fatalf("expected regional_outage rejection, got %+v", res.Rejections)
	}
	if !strings.Contains(res.Rejections[0].Detail, "was running before the burst") {
		t.Errorf("rejection detail should mention pre-burst survivor; got %q", res.Rejections[0].Detail)
	}
}

// TestDetect_StillAliveSurvivorRejected: the survivor's EndedAt is nil
// (launch is currently running). Same disqualification.
func TestDetect_StillAliveSurvivorRejected(t *testing.T) {
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-3*time.Minute)),
	}
	// Survivor still alive — no EndedAt.
	survivor := running(99, "vastai", T0.Add(-30*time.Minute))
	res := Detect(Config{}, failures, []*db.Launch{survivor}, T0.Add(30*time.Minute))
	if res.Incident != nil {
		t.Fatalf("expected nil incident (still-alive survivor), got %+v", res.Incident)
	}
	if len(res.Rejections) == 0 || res.Rejections[0].Reason != "regional_outage" {
		t.Fatalf("expected regional_outage rejection, got %+v", res.Rejections)
	}
}

// TestDetect_PreBurstLaunchThatDiedInBurstNotASurvivor: a launch that
// reached running before the burst but was itself killed during the
// burst window is NOT a survivor — it's a credit victim. Should not
// disqualify.
func TestDetect_PreBurstLaunchThatDiedInBurstNotASurvivor(t *testing.T) {
	failures := []*db.Launch{
		failed(1, "vastai", T0),
		failed(2, "vastai", T0.Add(-1*time.Minute)),
		failed(3, "vastai", T0.Add(-3*time.Minute)),
	}
	// "Survivor candidate" that actually died during the burst.
	victim := runningEnded(99, "vastai",
		T0.Add(-30*time.Minute), // started before burst
		T0.Add(-2*time.Minute),  // died during burst window
	)
	recovery := running(100, "vastai", T0.Add(10*time.Minute))
	res := Detect(Config{}, failures, []*db.Launch{victim, recovery}, T0.Add(30*time.Minute))
	if res.Incident == nil {
		t.Fatalf("expected incident (victim is not a survivor), got nil; rejections=%+v", res.Rejections)
	}
}

func TestConfig_WithDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.Lookback != DefaultLookback {
		t.Errorf("Lookback = %v, want %v", got.Lookback, DefaultLookback)
	}
	if got.BurstMin != DefaultBurstMin {
		t.Errorf("BurstMin = %d, want %d", got.BurstMin, DefaultBurstMin)
	}
	if got.BurstWindow != DefaultBurstWindow {
		t.Errorf("BurstWindow = %v, want %v", got.BurstWindow, DefaultBurstWindow)
	}
	if got.LeftPad != DefaultLeftPad {
		t.Errorf("LeftPad = %v, want %v", got.LeftPad, DefaultLeftPad)
	}
	if got.MinSilence != DefaultMinSilence {
		t.Errorf("MinSilence = %v, want %v", got.MinSilence, DefaultMinSilence)
	}
}

func TestConfig_OverridesRespected(t *testing.T) {
	cfg := Config{BurstMin: 5, BurstWindow: 30 * time.Second}.withDefaults()
	if cfg.BurstMin != 5 {
		t.Errorf("BurstMin override lost: got %d", cfg.BurstMin)
	}
	if cfg.BurstWindow != 30*time.Second {
		t.Errorf("BurstWindow override lost: got %v", cfg.BurstWindow)
	}
	if cfg.Lookback != DefaultLookback {
		t.Errorf("Lookback default lost: got %v", cfg.Lookback)
	}
}
