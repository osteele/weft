package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

func TestExtractGPUFromEnvVars(t *testing.T) {
	tests := []struct {
		name    string
		envVars []string
		want    string
	}{
		{
			name:    "no env vars",
			envVars: nil,
			want:    "",
		},
		{
			name:    "empty env vars",
			envVars: []string{},
			want:    "",
		},
		{
			name:    "no CUDA var",
			envVars: []string{"FOO=bar", "BATCH_SIZE=32"},
			want:    "",
		},
		{
			name:    "CUDA var only",
			envVars: []string{"CUDA_VISIBLE_DEVICES=0"},
			want:    "0",
		},
		{
			name:    "CUDA var with others",
			envVars: []string{"FOO=bar", "CUDA_VISIBLE_DEVICES=2", "BATCH_SIZE=32"},
			want:    "2",
		},
		{
			name:    "multiple GPUs",
			envVars: []string{"CUDA_VISIBLE_DEVICES=0,1,2"},
			want:    "0,1,2",
		},
		{
			name:    "CUDA var first",
			envVars: []string{"CUDA_VISIBLE_DEVICES=5", "OTHER=value"},
			want:    "5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractGPUFromEnvVars(tt.envVars)
			if got != tt.want {
				t.Errorf("extractGPUFromEnvVars(%v) = %q, want %q", tt.envVars, got, tt.want)
			}
		})
	}
}

func TestRunDescribeUpdatesDraftLocally(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordDraftJob(database, "", "/tmp/project", "echo old", "draft", "", "")
	if err != nil {
		t.Fatalf("record draft job: %v", err)
	}

	resetDescribeState()
	cmd := newDescribeTestCommand()
	if err := cmd.Flags().Set("message", "edited draft"); err != nil {
		t.Fatalf("set message flag: %v", err)
	}
	if err := cmd.Flags().Set("command", "echo new"); err != nil {
		t.Fatalf("set command flag: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runDescribe(cmd, []string{fmt.Sprintf("%d", jobID)}); err != nil {
			t.Fatalf("runDescribe: %v", err)
		}
	})
	if !strings.Contains(out, fmt.Sprintf("Updated job %s", ids.FormatJobID(jobID))) {
		t.Fatalf("output missing update, got %q", out)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.EffectiveStatus() != db.StatusDraft {
		t.Fatalf("status = %q, want draft", job.EffectiveStatus())
	}
	if job.Command != "echo new" || job.Description != "edited draft" {
		t.Fatalf("job fields = command %q description %q", job.Command, job.Description)
	}
}

func TestRunDescribeRejectsExternalExecutionFieldMutation(t *testing.T) {
	database := db.SetupTestDB(t)
	binding, _, err := db.UpsertExternalJobFromObservation(database, db.ExternalJobObservation{
		Executor: db.ExternalExecutorSkyPilot, ExternalJobID: "42",
		RawStatus: "PENDING", NormalizedStatus: db.StatusQueued, Command: "echo original",
	})
	if err != nil {
		t.Fatalf("create external job: %v", err)
	}
	resetDescribeState()
	t.Cleanup(resetDescribeState)
	cmd := newDescribeTestCommand()
	if err := cmd.Flags().Set("command", "echo changed"); err != nil {
		t.Fatalf("set command: %v", err)
	}
	err = runDescribe(cmd, []string{fmt.Sprintf("%d", binding.JobID)})
	if err == nil || !strings.Contains(err.Error(), "SkyPilot execution fields") {
		t.Fatalf("runDescribe external error = %v", err)
	}
	job, err := db.GetJobByID(database, binding.JobID)
	if err != nil {
		t.Fatalf("reload job: %v", err)
	}
	if job.Command != "echo original" {
		t.Fatalf("external command mutated to %q", job.Command)
	}
}

func resetDescribeState() {
	describeMessage = ""
	describeProject = ""
	describeDirectory = ""
	describeCommand = ""
	describeGPU = ""
	describeGPUs = ""
	describeGPUMem = 0
	describeCPU = 0
	describeGPUClass = ""
	describeProvider = ""
}

func newDescribeTestCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "describe <job-id>"}
	cmd.Flags().StringVarP(&describeMessage, "message", "m", "", "Set job description")
	cmd.Flags().StringVar(&describeProject, "project", "", "Set project name")
	cmd.Flags().StringVarP(&describeDirectory, "directory", "C", "", "Set working directory")
	cmd.Flags().StringVar(&describeCommand, "command", "", "Set command")
	cmd.Flags().StringVar(&describeGPU, "gpu", "", "Set GPU")
	cmd.Flags().StringVar(&describeGPUs, "gpus", "", "Set GPUs")
	cmd.Flags().IntVar(&describeGPUMem, "gpu-mem", 0, "Set GPU memory reservation in GB per device")
	cmd.Flags().IntVar(&describeCPU, "cpu", 0, "Set CPU allotment percent")
	cmd.Flags().StringVar(&describeGPUClass, "gpu-class", "", "GPU class or generation")
	cmd.Flags().StringVar(&describeProvider, "provider", "", "Cloud provider preference for rental placement")
	return cmd
}

func TestUpdateCudaVisibleDevices(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		gpuValue string
		want     string
	}{
		{
			name:     "add GPU to simple command",
			cmd:      "python train.py",
			gpuValue: "0",
			want:     "CUDA_VISIBLE_DEVICES=0 python train.py",
		},
		{
			name:     "add multiple GPUs",
			cmd:      "python train.py",
			gpuValue: "0,1,2",
			want:     "CUDA_VISIBLE_DEVICES=0,1,2 python train.py",
		},
		{
			name:     "replace existing GPU at start",
			cmd:      "CUDA_VISIBLE_DEVICES=0 python train.py",
			gpuValue: "1",
			want:     "CUDA_VISIBLE_DEVICES=1 python train.py",
		},
		{
			name:     "replace existing GPU with export",
			cmd:      "export CUDA_VISIBLE_DEVICES=0 && python train.py",
			gpuValue: "2",
			want:     "CUDA_VISIBLE_DEVICES=2 python train.py",
		},
		{
			name:     "replace multiple GPUs",
			cmd:      "CUDA_VISIBLE_DEVICES=0,1 python train.py",
			gpuValue: "2,3",
			want:     "CUDA_VISIBLE_DEVICES=2,3 python train.py",
		},
		{
			name:     "command with cd prefix",
			cmd:      "cd /foo && python train.py",
			gpuValue: "0",
			want:     "CUDA_VISIBLE_DEVICES=0 cd /foo && python train.py",
		},
		{
			name:     "replace GPU with cd prefix",
			cmd:      "CUDA_VISIBLE_DEVICES=1 cd /foo && python train.py",
			gpuValue: "0",
			want:     "CUDA_VISIBLE_DEVICES=0 cd /foo && python train.py",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := updateCudaVisibleDevices(tt.cmd, tt.gpuValue)
			if got != tt.want {
				t.Errorf("updateCudaVisibleDevices(%q, %q) = %q, want %q", tt.cmd, tt.gpuValue, got, tt.want)
			}
		})
	}
}
