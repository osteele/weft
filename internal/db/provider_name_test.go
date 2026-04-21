package db

import "testing"

func TestJobProviderName(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want string
	}{
		{"no tags", nil, ""},
		{"vastai tag", []string{"provider:vastai"}, "vastai"},
		{"runpod tag", []string{"provider:runpod"}, "runpod"},
		{"mixed with other tags", []string{"benchmark", "provider:runpod", "rental"}, "runpod"},
		{"case-insensitive prefix", []string{"Provider:VastAI"}, "vastai"},
		{"unknown provider value", []string{"provider:gcp"}, ""},
		{"no provider tag", []string{"benchmark", "rental"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Tags: tt.tags}
			if got := job.ProviderName(); got != tt.want {
				t.Errorf("ProviderName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJobProviderName_NilJob(t *testing.T) {
	var job *Job
	if got := job.ProviderName(); got != "" {
		t.Errorf("ProviderName() on nil = %q, want empty", got)
	}
}
