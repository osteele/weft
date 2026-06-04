package cmd

import (
	"reflect"
	"testing"
)

func TestNormalizeProviderFlag(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "vastai", input: "vastai", want: "vastai"},
		{name: "runpod uppercase", input: "RunPod", want: "runpod"},
		{name: "empty", input: "", want: ""},
		{name: "unknown", input: "foo", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeProviderFlag(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeProviderFlag(%q) expected error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeProviderFlag(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeProviderFlag(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestWithProviderTag(t *testing.T) {
	tags, err := withProviderTag([]string{"benchmark-isolation", "provider:vastai", "exp-1"}, "runpod")
	if err != nil {
		t.Fatalf("withProviderTag: %v", err)
	}
	want := []string{"benchmark-isolation", "exp-1", "provider:runpod"}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}

	tags, err = withProviderTag([]string{"provider:runpod", "exp-1"}, "")
	if err != nil {
		t.Fatalf("withProviderTag clear: %v", err)
	}
	want = []string{"exp-1"}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("cleared tags = %v, want %v", tags, want)
	}
}
