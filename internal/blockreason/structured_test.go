package blockreason

import (
	"strings"
	"testing"
)

func TestStructuredIsPlacementFailure(t *testing.T) {
	tests := []struct {
		name string
		s    *Structured
		want bool
	}{
		{name: "nil", s: nil, want: false},
		{name: "single-cause", s: &Structured{Summary: "waiting for output"}, want: false},
		{name: "launch only", s: &Structured{Launch: "no rental headroom"}, want: true},
		{
			name: "reuse only",
			s:    &Structured{Reuse: []ReuseRejection{{Instance: "wi1", Reason: "disk insufficient"}}},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.IsPlacementFailure(); got != tc.want {
				t.Fatalf("IsPlacementFailure() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStructuredReuseHeadline(t *testing.T) {
	tests := []struct {
		name string
		s    *Structured
		want string
	}{
		{name: "no instances", s: &Structured{Launch: "x"}, want: "no running instances to reuse"},
		{
			name: "one instance",
			s:    &Structured{Reuse: []ReuseRejection{{Instance: "wi1", Reason: "disk insufficient"}}},
			want: "reuse: disk insufficient",
		},
		{
			name: "three instances",
			s: &Structured{Reuse: []ReuseRejection{
				{Instance: "wi1", Reason: "disk insufficient"},
				{Instance: "wi2", Reason: "GPU class mismatch"},
				{Instance: "wi3", Reason: "grace period too short"},
			}},
			want: "reuse: disk insufficient + 2 more instances",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.ReuseHeadline(); got != tc.want {
				t.Fatalf("ReuseHeadline() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStructuredDetailLines(t *testing.T) {
	s := &Structured{
		Summary: "no rental headroom; running instances couldn't accept this job",
		Launch:  "no rental headroom",
		Reuse: []ReuseRejection{
			{Instance: "wi1", Reason: "disk insufficient: need=42GB free=12GB"},
			{Instance: "wi2", Reason: "GPU class mismatch: job=ampere instance=ada"},
		},
	}
	lines := s.DetailLines()
	if len(lines) != 3 {
		t.Fatalf("DetailLines() returned %d lines, want 3: %q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "new instance") || !strings.Contains(lines[0], "no rental headroom") {
		t.Fatalf("line 0 = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "reuse wi1") || !strings.Contains(lines[1], "disk insufficient") {
		t.Fatalf("line 1 = %q", lines[1])
	}

	single := &Structured{Summary: "waiting for output/x.pt"}
	if got := single.DetailLines(); len(got) != 1 || got[0] != "waiting for output/x.pt" {
		t.Fatalf("single-cause DetailLines() = %q", got)
	}
}

func TestStructuredMarshalRoundTrip(t *testing.T) {
	s := &Structured{
		Summary: "no rental headroom; running instances couldn't accept this job: disk insufficient",
		Launch:  "no rental headroom",
		Reuse:   []ReuseRejection{{Instance: "wi1", Reason: "disk insufficient: need=42GB free=12GB"}},
	}
	encoded := s.Marshal()
	if encoded == "" {
		t.Fatal("Marshal() returned empty string")
	}
	got := Parse(encoded)
	if got == nil {
		t.Fatal("Parse() returned nil")
	}
	if got.Summary != s.Summary || got.Launch != s.Launch || len(got.Reuse) != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.Reuse[0] != s.Reuse[0] {
		t.Fatalf("reuse round trip mismatch: %+v", got.Reuse[0])
	}
}

func TestStructuredParseInvalid(t *testing.T) {
	for _, in := range []string{"", "   ", "not json", "{bad"} {
		if got := Parse(in); got != nil {
			t.Fatalf("Parse(%q) = %+v, want nil", in, got)
		}
	}
}

func TestStructuredMarshalEmpty(t *testing.T) {
	if got := (*Structured)(nil).Marshal(); got != "" {
		t.Fatalf("nil Marshal() = %q, want empty", got)
	}
	if got := (&Structured{}).Marshal(); got != "" {
		t.Fatalf("empty Marshal() = %q, want empty", got)
	}
}

// TestStructuredDetailLinesExpandsLaunchDetail verifies that when the launch
// avenue carries a multi-line LaunchDetail (e.g. the full vastai stderr), the
// disclosure emits the compact reason on the avenue row and continues with the
// full detail on indented follow-on rows. The user must be able to see the
// underlying provider message, not a clipped one-liner.
func TestStructuredDetailLinesExpandsLaunchDetail(t *testing.T) {
	s := &Structured{
		Summary: "planner: search offers: provider rejected request: 400 invalid filter",
		Launch:  "planner: search offers: provider rejected request: 400 invalid filter",
		LaunchDetail: "search offers: provider rejected request: Warning: ignoring legacy parameter\n" +
			"{\"error\": true, \"status_code\": 400, \"msg\": \"bogus_field is not a valid search key\"}",
	}
	lines := s.DetailLines()
	if len(lines) < 3 {
		t.Fatalf("DetailLines() returned %d lines, want >=3 (avenue + multi-line detail): %q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "new instance") {
		t.Fatalf("avenue row = %q, want prefix \"new instance\"", lines[0])
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "bogus_field is not a valid search key") {
		t.Fatalf("DetailLines() = %q, want full stderr line preserved", joined)
	}
	if !strings.Contains(joined, "Warning: ignoring legacy parameter") {
		t.Fatalf("DetailLines() = %q, want first detail line preserved", joined)
	}
}

// TestStructuredDetailLinesSkipsRedundantDetail verifies the disclosure does
// not emit a follow-on row when LaunchDetail matches the compact reason —
// the compact line already says everything.
func TestStructuredDetailLinesSkipsRedundantDetail(t *testing.T) {
	s := &Structured{
		Summary:      "no offers from providers for nvidia >=24GB",
		Launch:       "no offers from providers for nvidia >=24GB",
		LaunchDetail: "no offers from providers for nvidia >=24GB",
	}
	lines := s.DetailLines()
	if len(lines) != 1 {
		t.Fatalf("DetailLines() returned %d lines for redundant detail, want 1: %q", len(lines), lines)
	}
}

// TestStructuredMarshalRoundTripIncludesFingerprint verifies the
// error-coalescing fingerprint survives Marshal/Parse. Required so the TUI
// disclosure and `weft incidents` CLI can group jobs by fingerprint after a
// process restart without re-running the planner.
func TestStructuredMarshalRoundTripIncludesFingerprint(t *testing.T) {
	s := &Structured{
		Summary:     "planner: search offers: provider rejected request",
		Launch:      "planner: search offers: provider rejected request",
		Fingerprint: "vastai/search-offers/400/bad-field:driver_vers",
	}
	encoded := s.Marshal()
	got := Parse(encoded)
	if got == nil {
		t.Fatal("Parse returned nil")
	}
	if got.Fingerprint != s.Fingerprint {
		t.Fatalf("Fingerprint round trip: got %q, want %q", got.Fingerprint, s.Fingerprint)
	}
}

// TestStructuredParseBackwardCompatibleNoFingerprint verifies that pre-existing
// rows in the placement_blocked column (persisted before this commit added
// Fingerprint) parse cleanly with an empty Fingerprint field.
func TestStructuredParseBackwardCompatibleNoFingerprint(t *testing.T) {
	// Shape from a pre-fingerprint DB row.
	encoded := `{"summary":"planner: search offers: provider rejected request","launch":"planner: search offers: provider rejected request"}`
	got := Parse(encoded)
	if got == nil {
		t.Fatal("Parse returned nil for pre-fingerprint row")
	}
	if got.Fingerprint != "" {
		t.Fatalf("Fingerprint = %q, want empty for pre-fingerprint row", got.Fingerprint)
	}
	if !got.IsPlacementFailure() {
		t.Fatal("pre-fingerprint row lost IsPlacementFailure")
	}
}

// TestStructuredMarshalRoundTripIncludesDetail verifies that LaunchDetail and
// ReuseRejection.Detail are persisted across Marshal/Parse so the placement
// disclosure survives a process restart.
func TestStructuredMarshalRoundTripIncludesDetail(t *testing.T) {
	s := &Structured{
		Summary:      "planner: search offers: provider rejected request",
		Launch:       "planner: search offers: provider rejected request",
		LaunchDetail: "search offers: provider rejected request: stderr line 1\nstderr line 2",
		Reuse: []ReuseRejection{
			{Instance: "wi1", Reason: "disk insufficient", Detail: "need=42GB free=12GB max=24GB"},
		},
	}
	encoded := s.Marshal()
	got := Parse(encoded)
	if got == nil {
		t.Fatal("Parse() returned nil")
	}
	if got.LaunchDetail != s.LaunchDetail {
		t.Fatalf("LaunchDetail round trip: got %q, want %q", got.LaunchDetail, s.LaunchDetail)
	}
	if len(got.Reuse) != 1 || got.Reuse[0].Detail != s.Reuse[0].Detail {
		t.Fatalf("ReuseRejection.Detail round trip: got %+v, want %+v", got.Reuse, s.Reuse)
	}
}

// OnPrem persists the per-host on-prem rejection detail (previously
// oplog-only) and surfaces it as its own avenue.
func TestStructuredOnPremAvenue(t *testing.T) {
	s := &Structured{
		Summary: "no rental headroom; running instances couldn't accept this job",
		Launch:  "no rental headroom",
		OnPrem:  "2 hosts: cool30: driver floor: NVIDIA driver 525.125.06 < required >=570; cool100: host overloaded",
	}
	data := s.Marshal()
	round := Parse(data)
	if round == nil || round.OnPrem != s.OnPrem {
		t.Fatalf("OnPrem did not round-trip: %+v", round)
	}
	if !round.IsPlacementFailure() {
		t.Fatal("IsPlacementFailure = false with OnPrem set")
	}
	lines := round.DetailLines()
	found := false
	for _, l := range lines {
		if strings.Contains(l, "on-prem hosts") && strings.Contains(l, "cool30") {
			found = true
		}
	}
	if !found {
		t.Fatalf("DetailLines missing on-prem avenue: %v", lines)
	}
}

// Recorded reuse outcomes overlay re-probed ones: same-instance entries are
// replaced (the recorded failure is what actually happened), new instances
// are prepended.
func TestMergeRecordedReuse(t *testing.T) {
	s := &Structured{
		Summary: "x",
		Launch:  "no rental headroom",
		Reuse: []ReuseRejection{
			{Instance: "wi1", Reason: "probe says compatible-ish"},
			{Instance: "wi2", Reason: "GPU memory insufficient"},
		},
	}
	s.MergeRecordedReuse([]ReuseRejection{
		{Instance: "wi1", Reason: "submit failed: upload source: connection reset", Detail: "full err"},
		{Instance: "wi3", Reason: "submit failed: no ack"},
	})
	if len(s.Reuse) != 3 {
		t.Fatalf("reuse entries = %d, want 3", len(s.Reuse))
	}
	if s.Reuse[0].Instance != "wi3" {
		t.Errorf("new recorded instance not prepended: %+v", s.Reuse)
	}
	var wi1 *ReuseRejection
	for i := range s.Reuse {
		if s.Reuse[i].Instance == "wi1" {
			wi1 = &s.Reuse[i]
		}
	}
	if wi1 == nil || !strings.Contains(wi1.Reason, "submit failed") {
		t.Errorf("recorded outcome did not replace probe entry for wi1: %+v", s.Reuse)
	}
}

func TestStructuredLastAttemptSurfaces(t *testing.T) {
	s := &Structured{
		Summary:     "no launch path determined; running instances couldn't accept this job",
		Launch:      "no launch path determined",
		LastAttempt: "last instance wi5099 terminated (infrastructure failure) — job requeued, awaiting relaunch",
	}
	if !s.IsPlacementFailure() {
		t.Fatal("IsPlacementFailure() = false, want true when LastAttempt set")
	}
	lines := s.DetailLines()
	var foundLast bool
	for _, ln := range lines {
		if strings.HasPrefix(ln, "last attempt") && strings.Contains(ln, "wi5099") && strings.Contains(ln, "awaiting relaunch") {
			foundLast = true
		}
	}
	if !foundLast {
		t.Fatalf("DetailLines() missing last-attempt avenue: %q", lines)
	}
	got := Parse(s.Marshal())
	if got == nil || got.LastAttempt != s.LastAttempt {
		t.Fatalf("LastAttempt round trip mismatch: %+v", got)
	}
}

func TestStructuredLastAttemptOnlyIsPlacementFailure(t *testing.T) {
	s := &Structured{LastAttempt: "last instance wi1 terminated (bootstrap timeout) — job requeued, awaiting relaunch"}
	if !s.IsPlacementFailure() {
		t.Fatal("IsPlacementFailure() = false, want true for LastAttempt-only")
	}
	lines := s.DetailLines()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "last attempt") {
		t.Fatalf("DetailLines() = %q, want single last-attempt line", lines)
	}
}
