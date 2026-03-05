package cloud

import (
	"strings"
	"testing"
)

func TestGenerateWrapper_UsesClientWorkspacePath(t *testing.T) {
	client := &MockClient{
		WorkspacePathVal:   "/runpod-volume/",
		SelfDestructCmdVal: `runpodctl remove pod "$RUNPOD_POD_ID" 2>/dev/null || true`,
	}

	wrapper := GenerateWrapper(client, 42, "python train.py", "my-bucket")

	if !strings.Contains(wrapper, "cd /runpod-volume/") {
		t.Error("wrapper should use client's workspace path")
	}
	if !strings.Contains(wrapper, "runpodctl remove pod") {
		t.Error("wrapper should use client's self-destruct command")
	}
	if strings.Contains(wrapper, "vastai destroy") {
		t.Error("wrapper should not contain vastai-specific commands")
	}
}

func TestGenerateWrapper_VastaiDefaults(t *testing.T) {
	client := &MockClient{
		WorkspacePathVal:   "/workspace/",
		SelfDestructCmdVal: `vastai destroy instance "$CONTAINER_ID" --api-key "$CONTAINER_API_KEY" 2>/dev/null || true`,
	}

	wrapper := GenerateWrapper(client, 99, "echo hello", "test-bucket")

	if !strings.Contains(wrapper, "cd /workspace/") {
		t.Error("wrapper should use /workspace/")
	}
	if !strings.Contains(wrapper, "vastai destroy instance") {
		t.Error("wrapper should use vastai self-destruct")
	}
	if !strings.Contains(wrapper, `JOB_ID="99"`) {
		t.Error("wrapper should contain job ID")
	}
}

func TestGenerateCampaignWrapper_MultiJob(t *testing.T) {
	client := &MockClient{
		WorkspacePathVal:   "/workspace/",
		SelfDestructCmdVal: `vastai destroy instance "$CONTAINER_ID" --api-key "$CONTAINER_API_KEY" 2>/dev/null || true`,
	}

	jobs := []CampaignJob{
		{ID: 10, Command: "python train.py"},
		{ID: 11, Command: "python eval.py"},
	}

	wrapper := GenerateCampaignWrapper(client, 5, jobs, "my-bucket", false)

	if !strings.Contains(wrapper, "JOB_ID=10") {
		t.Error("wrapper should contain first job ID")
	}
	if !strings.Contains(wrapper, "JOB_ID=11") {
		t.Error("wrapper should contain second job ID")
	}
	if !strings.Contains(wrapper, "CAMPAIGN_ID=5") {
		t.Error("wrapper should contain campaign ID")
	}
	if !strings.Contains(wrapper, "CAMPAIGN_FAILED") {
		t.Error("wrapper should track campaign failure state")
	}
}
