package ids

import "testing"

func TestFormatJobID(t *testing.T) {
	if got := FormatJobID(750); got != "wj750" {
		t.Fatalf("FormatJobID(750) = %q, want %q", got, "wj750")
	}
}

func TestParseJobID(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "numeric", raw: "750", want: 750},
		{name: "prefixed lowercase", raw: "wj750", want: 750},
		{name: "prefixed uppercase", raw: "WJ750", want: 750},
		{name: "prefixed missing suffix", raw: "wj", wantErr: true},
		{name: "invalid", raw: "abc", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
		{name: "whitespace trimmed", raw: "  wj42  ", want: 42},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseJobID(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseJobID(%q) = %d, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseJobID(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("ParseJobID(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestFormatJobIDListCompact(t *testing.T) {
	tests := []struct {
		name string
		in   []int64
		want string
	}{
		{name: "empty", in: nil, want: ""},
		{name: "single", in: []int64{750}, want: "wj750"},
		{name: "consecutive collapses to range", in: []int64{750, 751, 752}, want: "wj750:wj752"},
		{name: "mixed", in: []int64{750, 751, 752, 760}, want: "wj750:wj752,wj760"},
		{name: "out-of-order sorted", in: []int64{760, 750, 752, 751}, want: "wj750:wj752,wj760"},
		{name: "duplicates deduplicated via range", in: []int64{750, 750, 751}, want: "wj750:wj751"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatJobIDListCompact(tt.in); got != tt.want {
				t.Errorf("FormatJobIDListCompact(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
