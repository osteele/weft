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
