package campaign

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestValidateGroupCloudProvisionable(t *testing.T) {
	tests := []struct {
		name      string
		jobs      []*db.Job
		wantErr   bool
		errSubstr string
	}{
		{
			name: "checkpoint input is rejected",
			jobs: []*db.Job{
				{ID: 2028, Inputs: []string{"checkpoint:adaptive-escalation/gsm8k-7b-traces"}},
			},
			wantErr:   true,
			errSubstr: "checkpoint:adaptive-escalation/gsm8k-7b-traces",
		},
		{
			name: "corpus input is rejected",
			jobs: []*db.Job{
				{ID: 7, Inputs: []string{"corpus:penn-treebank/conllu"}},
			},
			wantErr:   true,
			errSubstr: "corpus",
		},
		{
			name: "hf and hf-dataset inputs are allowed",
			jobs: []*db.Job{
				{ID: 1, Inputs: []string{"hf:meta-llama/Llama-3-8B", "hf-dataset:wikitext"}},
			},
		},
		{
			name: "local and plain file-path inputs are allowed",
			jobs: []*db.Job{
				{ID: 2, Inputs: []string{"local:data/", "~/sources/vidur/data/"}},
			},
		},
		{
			name: "job-output input is allowed (co-located or staged via --needs)",
			jobs: []*db.Job{
				{ID: 3, Inputs: []string{"job-output:4823/outputs"}},
			},
		},
		{
			name: "empty group is allowed",
			jobs: nil,
		},
		{
			name: "checkpoint among other jobs is rejected and names the offender",
			jobs: []*db.Job{
				{ID: 100, Inputs: []string{"hf:meta-llama/Llama-3-8B"}},
				{ID: 101, Inputs: []string{"checkpoint:LM2/runs/gpt2-ft-v1"}},
			},
			wantErr:   true,
			errSubstr: "wj101",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGroupCloudProvisionable(InstanceGroup{Jobs: tc.jobs})
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
			} else if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}
