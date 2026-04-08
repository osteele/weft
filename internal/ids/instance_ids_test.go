package ids

import (
	"reflect"
	"testing"
)

func TestFormatInstanceID(t *testing.T) {
	if got := FormatInstanceID(123); got != "wi123" {
		t.Fatalf("FormatInstanceID(123) = %q, want %q", got, "wi123")
	}
}

func TestParseInstanceID(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    int64
		wantErr bool
	}{
		{name: "numeric", raw: "123", want: 123},
		{name: "prefixed lowercase", raw: "wi123", want: 123},
		{name: "prefixed uppercase", raw: "WI123", want: 123},
		{name: "prefixed missing suffix", raw: "wi", wantErr: true},
		{name: "invalid", raw: "abc", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInstanceID(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseInstanceID(%q) = %d, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseInstanceID(%q) error = %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("ParseInstanceID(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestFormatInstanceIDList(t *testing.T) {
	got := FormatInstanceIDList([]int64{3, 9})
	want := []string{"wi3", "wi9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FormatInstanceIDList() = %v, want %v", got, want)
	}
}
