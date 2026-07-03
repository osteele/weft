package campaign

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
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
			database := db.SetupTestDB(t)
			err := validateGroupCloudProvisionable(context.Background(), InstanceGroup{Jobs: tc.jobs}, database, &r2.Client{})
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

func TestValidateGroupCloudProvisionable_CheckpointTransportabilityStates(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := dataloc.RecordAsset(database, dataloc.HostDataEntry{
		Host:        "host-beta",
		Asset:       dataloc.DataAsset{Kind: dataloc.AssetCheckpoint, ID: "trace"},
		Path:        "data/trace",
		SizeBytes:   1234,
		ContentHash: strings.Repeat("a", 64),
		ContentType: dataloc.ContentTypeDirectory,
		LastSeen:    time.Now(),
	}); err != nil {
		t.Fatalf("RecordAsset: %v", err)
	}
	job := &db.Job{ID: 44, Inputs: []string{"checkpoint:trace"}}

	prev := checkpointObjectExistsFunc
	t.Cleanup(func() { checkpointObjectExistsFunc = prev })

	checkpointObjectExistsFunc = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return true, nil
	}
	if err := validateGroupCloudProvisionable(context.Background(), InstanceGroup{Jobs: []*db.Job{job}}, database, &r2.Client{}); err != nil {
		t.Fatalf("confirmed R2 checkpoint should be provisionable: %v", err)
	}

	checkpointObjectExistsFunc = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return false, nil
	}
	err := validateGroupCloudProvisionable(context.Background(), InstanceGroup{Jobs: []*db.Job{job}}, database, &r2.Client{})
	if err == nil || !strings.Contains(err.Error(), "not confirmed in R2") {
		t.Fatalf("confirmed absent err = %v", err)
	}

	checkpointObjectExistsFunc = func(_ context.Context, _ *r2.Client, _ string) (bool, error) {
		return false, fmt.Errorf("timeout")
	}
	err = validateGroupCloudProvisionable(context.Background(), InstanceGroup{Jobs: []*db.Job{job}}, database, &r2.Client{})
	if err == nil || !strings.Contains(err.Error(), "could not verify") {
		t.Fatalf("unknown err = %v", err)
	}
}
