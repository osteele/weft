package vastai

import (
	"strings"
	"testing"
)

func TestGenerateCampaignWrapper(t *testing.T) {
	jobs := []CampaignJob{
		{ID: 42, Command: "python train.py"},
		{ID: 43, Command: "python eval.py"},
	}

	wrapper := GenerateCampaignWrapper(1, jobs, "my-bucket", false)

	if !strings.Contains(wrapper, "CAMPAIGN_ID=1") {
		t.Error("wrapper should contain campaign ID")
	}
	if !strings.Contains(wrapper, "R2_BUCKET=\"my-bucket\"") {
		t.Error("wrapper should contain R2 bucket")
	}
	if !strings.Contains(wrapper, "JOB_ID=42") {
		t.Error("wrapper should contain job 42")
	}
	if !strings.Contains(wrapper, "JOB_ID=43") {
		t.Error("wrapper should contain job 43")
	}
	if !strings.Contains(wrapper, "CAMPAIGN_FAILED") {
		t.Error("wrapper should track failure state")
	}
	// With continueOnFailure=false, should have conditional blocks
	if !strings.Contains(wrapper, "if [ $CAMPAIGN_FAILED -eq 0 ]") {
		t.Error("wrapper should guard jobs when continueOnFailure=false")
	}
	if !strings.Contains(wrapper, "campaigns/$CAMPAIGN_ID/.complete") {
		t.Error("wrapper should write campaign completion marker")
	}
}

func TestGenerateCampaignWrapper_ContinueOnFailure(t *testing.T) {
	jobs := []CampaignJob{
		{ID: 1, Command: "echo hello"},
	}

	wrapper := GenerateCampaignWrapper(99, jobs, "bucket", true)

	if strings.Contains(wrapper, "if [ $CAMPAIGN_FAILED -eq 0 ]") {
		t.Error("wrapper should not guard jobs when continueOnFailure=true")
	}
}
