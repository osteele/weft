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

func TestNormalizeRunpodCloudTypeFlag(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "secure", input: "secure", want: "secure"},
		{name: "community cloud", input: "community-cloud", want: "community"},
		{name: "clear", input: "clear", want: ""},
		{name: "default", input: "default", want: ""},
		{name: "unknown", input: "private", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeRunpodCloudTypeFlag(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeRunpodCloudTypeFlag(%q) expected error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeRunpodCloudTypeFlag(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("normalizeRunpodCloudTypeFlag(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestApplyRunpodCloudTypeProviderIntent(t *testing.T) {
	tags, err := applyRunpodCloudTypeProviderIntent([]string{"benchmark"}, "", "secure")
	if err != nil {
		t.Fatalf("applyRunpodCloudTypeProviderIntent: %v", err)
	}
	want := []string{"benchmark", "provider:runpod"}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}

	if _, err := applyRunpodCloudTypeProviderIntent([]string{"provider:vastai"}, "", "secure"); err == nil {
		t.Fatal("expected provider conflict error")
	}
	if _, err := applyRunpodCloudTypeProviderIntent(nil, "cool30", "secure"); err == nil {
		t.Fatal("expected inventory host conflict error")
	}
}
