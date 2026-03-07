package cloud

import (
	"strings"
	"testing"
)

func TestGenerateAgentWrapper_UsesClientWorkspacePath(t *testing.T) {
	client := &MockClient{
		WorkspacePathVal:   "/runpod-volume/",
		SelfDestructCmdVal: `runpodctl remove pod "$RUNPOD_POD_ID" 2>/dev/null || true`,
	}

	jobs := []AgentJob{{ID: 42, Command: "python train.py"}}
	wrapper := GenerateAgentWrapper(client, jobs, "my-bucket", "12345", WrapperOpts{})

	if !strings.Contains(wrapper, "--working-dir=/runpod-volume/") {
		t.Error("wrapper should use client's workspace path as default working dir")
	}
	if !strings.Contains(wrapper, "runpodctl remove pod") {
		t.Error("wrapper should use client's self-destruct command")
	}
}

func TestGenerateAgentWrapper_MultiJob(t *testing.T) {
	client := &MockClient{
		WorkspacePathVal:   "/workspace/",
		SelfDestructCmdVal: `vastai destroy instance "$CONTAINER_ID" --api-key "$CONTAINER_API_KEY" 2>/dev/null || true`,
	}

	jobs := []AgentJob{
		{ID: 10, Command: "python train.py"},
		{ID: 11, Command: "python eval.py", Dir: "/custom/dir"},
	}
	wrapper := GenerateAgentWrapper(client, jobs, "my-bucket", "12345", WrapperOpts{})

	if !strings.Contains(wrapper, "JOB_ID=10") {
		t.Error("wrapper should contain first job ID")
	}
	if !strings.Contains(wrapper, "JOB_ID=11") {
		t.Error("wrapper should contain second job ID")
	}
	if !strings.Contains(wrapper, "weft-agent run-job") {
		t.Error("wrapper should invoke weft-agent run-job")
	}
	if !strings.Contains(wrapper, "--working-dir=/workspace/") {
		t.Error("wrapper should use default workspace for jobs without Dir")
	}
	if !strings.Contains(wrapper, "--working-dir=/custom/dir") {
		t.Error("wrapper should use job-specific Dir when set")
	}
	if !strings.Contains(wrapper, "vastai destroy instance") {
		t.Error("wrapper should self-destruct")
	}
}

func TestGenerateAgentWrapper_EnvVars(t *testing.T) {
	client := &MockClient{
		WorkspacePathVal:   "/workspace/",
		SelfDestructCmdVal: "true",
	}

	jobs := []AgentJob{{ID: 1, Command: "python train.py"}}
	wrapper := GenerateAgentWrapper(client, jobs, "bucket", "123", WrapperOpts{
		EnvVars: map[string]string{
			"HF_TOKEN":               "hf_test123",
			"HUGGING_FACE_HUB_TOKEN": "hf_test123",
		},
	})

	if !strings.Contains(wrapper, `export HF_TOKEN="hf_test123"`) {
		t.Error("wrapper should export HF_TOKEN")
	}
	if !strings.Contains(wrapper, `export HUGGING_FACE_HUB_TOKEN="hf_test123"`) {
		t.Error("wrapper should export HUGGING_FACE_HUB_TOKEN")
	}
}

func TestGenerateAgentWrapper_MaxTime(t *testing.T) {
	client := &MockClient{
		WorkspacePathVal:   "/workspace/",
		SelfDestructCmdVal: "true",
	}

	jobs := []AgentJob{
		{ID: 1, Command: "python train.py"},
		{ID: 2, Command: "python eval.py"},
	}
	wrapper := GenerateAgentWrapper(client, jobs, "bucket", "123", WrapperOpts{
		MaxTimeSeconds: 3600,
	})

	if !strings.Contains(wrapper, "MAX_SECONDS=3600") {
		t.Error("wrapper should set instance-level MAX_SECONDS")
	}
	if !strings.Contains(wrapper, "INSTANCE_START=") {
		t.Error("wrapper should record INSTANCE_START timestamp")
	}
	if !strings.Contains(wrapper, "REMAINING=$(( MAX_SECONDS - ELAPSED ))") {
		t.Error("wrapper should compute remaining time per job")
	}
	if !strings.Contains(wrapper, "--max-time=${REMAINING}s") {
		t.Error("wrapper should pass remaining time as --max-time to agent")
	}
	if !strings.Contains(wrapper, "Instance time budget exhausted") {
		t.Error("wrapper should skip remaining jobs when budget exhausted")
	}
	if !strings.Contains(wrapper, "run_jobs") {
		t.Error("wrapper should wrap jobs in a function for early return")
	}
}

func TestGenerateAgentWrapper_R2Upload(t *testing.T) {
	client := &MockClient{
		WorkspacePathVal:   "/workspace/",
		SelfDestructCmdVal: "true",
	}

	jobs := []AgentJob{{ID: 1, Command: "echo hi"}}
	wrapper := GenerateAgentWrapper(client, jobs, "test-bucket", "99999", WrapperOpts{})

	if !strings.Contains(wrapper, `R2_BUCKET="test-bucket"`) {
		t.Error("wrapper should set R2_BUCKET")
	}
	if !strings.Contains(wrapper, "rclone copy $LOG_DIR/") {
		t.Error("wrapper should upload results via rclone")
	}
	if !strings.Contains(wrapper, ".complete") {
		t.Error("wrapper should write completion marker")
	}
}
