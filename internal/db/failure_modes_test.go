package db

import "testing"

func TestClassifyInfraFailure(t *testing.T) {
	tests := []struct {
		name       string
		phase      FailurePhase
		exitCode   int
		logTail    string
		wantReason string
		wantInfra  bool
	}{
		{
			name:       "prewarm download failure is infra regardless of exit code",
			phase:      PhasePrewarmDownload,
			exitCode:   1,
			wantReason: FailureReasonInfraPrewarmDownloadFailed,
			wantInfra:  true,
		},
		{
			name:       "prewarm download timeout is infra",
			phase:      PhasePrewarmDownload,
			exitCode:   ExitCodeSetupTimeout,
			wantReason: FailureReasonInfraPrewarmDownloadFailed,
			wantInfra:  true,
		},
		{
			name:       "cloud artifact staging is infra",
			phase:      PhaseCloudArtifactStaging,
			exitCode:   1,
			logTail:    "r2 timeout",
			wantReason: FailureReasonInfraCloudArtifactStageFailed,
			wantInfra:  true,
		},
		{
			name:      "gpu count shortfall is infra",
			phase:     PhaseGPUCountPreflight,
			wantInfra: true,
		},
		{
			name:       "torch preflight environment failure is user attributed",
			phase:      PhaseTorchPreflightEnvironment,
			exitCode:   1,
			wantReason: FailureReasonTorchPreflightEnvironmentFailed,
		},
		{
			name:       "torch import failure is user attributed",
			phase:      PhaseTorchPreflightImport,
			exitCode:   1,
			wantReason: FailureReasonTorchPreflightImportFailed,
		},
		{
			name:       "torch CUDA probe failure is infra",
			phase:      PhaseTorchPreflightCUDA,
			exitCode:   1,
			logTail:    "cuda error: unknown error",
			wantReason: FailureReasonInfraTorchPreflightFailed,
			wantInfra:  true,
		},
		{
			name:      "setup timeout exit 124 is infra without reason override",
			phase:     PhaseSetup,
			exitCode:  ExitCodeSetupTimeout,
			wantInfra: true,
		},
		{
			name:     "setup generic failure stays user-attributed",
			phase:    PhaseSetup,
			exitCode: 1,
		},
		{
			name:       "runtime nvlink fault is infra",
			phase:      PhaseRuntime,
			exitCode:   1,
			logTail:    "invalid access of peer gpu memory over nvlink",
			wantReason: FailureReasonInfraCUDAHardwareFault,
			wantInfra:  true,
		},
		{
			name:       "runtime uncorrectable ecc fault is infra",
			phase:      PhaseRuntime,
			exitCode:   1,
			logTail:    "cuda error: uncorrectable ecc error encountered",
			wantReason: FailureReasonInfraCUDAHardwareFault,
			wantInfra:  true,
		},
		{
			name:       "runtime xid report is infra",
			phase:      PhaseRuntime,
			exitCode:   1,
			logTail:    "nvrm: xid (pci:0000:81:00): 79, gpu has fallen off the bus",
			wantReason: FailureReasonInfraCUDAHardwareFault,
			wantInfra:  true,
		},
		{
			name:     "runtime plain failure stays user-attributed",
			phase:    PhaseRuntime,
			exitCode: 1,
			logTail:  "traceback (most recent call last): valueerror",
		},
		{
			name:     "runtime setup-timeout exit code alone is not infra",
			phase:    PhaseRuntime,
			exitCode: ExitCodeSetupTimeout,
		},
		{
			name:     "unknown phase stays user-attributed",
			phase:    FailurePhase("bogus"),
			exitCode: ExitCodeSetupTimeout,
			logTail:  "nvlink",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, infra := ClassifyInfraFailure(tt.phase, tt.exitCode, tt.logTail)
			if reason != tt.wantReason || infra != tt.wantInfra {
				t.Fatalf("ClassifyInfraFailure(%q, %d, %q) = (%q, %v), want (%q, %v)",
					tt.phase, tt.exitCode, tt.logTail, reason, infra, tt.wantReason, tt.wantInfra)
			}
		})
	}
}

func TestIsInfraFailureReason(t *testing.T) {
	for reason, want := range map[string]bool{
		FailureReasonInfraPrewarmDownloadFailed:    true,
		FailureReasonInfraCloudArtifactStageFailed: true,
		FailureReasonInfraCUDAHardwareFault:        true,
		FailureReasonInfraTorchPreflightFailed:     true,
		"setup_timeout":                            false,
		"error":                                    false,
		"":                                         false,
	} {
		if got := IsInfraFailureReason(reason); got != want {
			t.Errorf("IsInfraFailureReason(%q) = %v, want %v", reason, got, want)
		}
	}
}

func TestInfraFailureReasonsMatchesClassifier(t *testing.T) {
	reasons := InfraFailureReasons()
	if len(reasons) == 0 {
		t.Fatal("InfraFailureReasons returned no reasons")
	}
	reasons[0] = "mutated"
	if IsInfraFailureReason("mutated") {
		t.Fatal("InfraFailureReasons returned mutable backing storage")
	}
	for _, reason := range InfraFailureReasons() {
		if !IsInfraFailureReason(reason) {
			t.Fatalf("InfraFailureReasons includes %q, but IsInfraFailureReason rejects it", reason)
		}
	}
}

func TestClassifyFailureMode(t *testing.T) {
	tests := []struct {
		name          string
		failureReason string
		diagnosis     string
		message       string
		exitCode      int
		want          string
	}{
		{name: "diagnosis pattern wins", diagnosis: `{"pattern":"gpu_oom"}`, want: "gpu_oom"},
		{name: "module text", message: "ModuleNotFoundError: No module named transformers", want: "module_not_found"},
		{name: "cuda fault text", message: "CUDA error: an illegal memory access was encountered", want: "cuda_fault"},
		{name: "cuda hardware text", message: "torch.AcceleratorError: CUDA error: Invalid access of peer GPU memory over nvlink or a hardware error", want: "cuda_hardware_fault"},
		{name: "cuda hardware reason", failureReason: FailureReasonInfraCUDAHardwareFault, want: "cuda_hardware_fault"},
		{name: "exit 137", exitCode: 137, want: "oom"},
		{name: "success empty", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyFailureMode(tt.failureReason, tt.diagnosis, tt.message, tt.exitCode)
			if got != tt.want {
				t.Fatalf("ClassifyFailureMode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPredictFailureModesUsesSpecificScope(t *testing.T) {
	database := setupTestDB(t)

	for i := 0; i < 3; i++ {
		jobID, err := RecordQueuedWithGPU(database, "host-a", "/tmp/test", "python train.py", "test", "a100")
		if err != nil {
			t.Fatalf("insert failed job: %v", err)
		}
		if _, err := database.Exec(
			`UPDATE job_attempts
			 SET error_diagnosis = ?, exit_code = 1, end_time = 2000
			 WHERE job_id = ? AND end_time IS NULL`,
			`{"pattern":"module_not_found"}`,
			jobID,
		); err != nil {
			t.Fatalf("update failed job: %v", err)
		}
	}
	jobID, err := RecordQueuedWithGPU(database, "host-a", "/tmp/test", "python train.py", "test", "a100")
	if err != nil {
		t.Fatalf("insert successful job: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET exit_code = 0, end_time = 2000 WHERE job_id = ? AND end_time IS NULL`,
		jobID,
	); err != nil {
		t.Fatalf("update successful job: %v", err)
	}

	preds, err := PredictFailureModes(database, "python train.py", "host-a", "a100")
	if err != nil {
		t.Fatalf("PredictFailureModes: %v", err)
	}
	if len(preds) == 0 {
		t.Fatal("expected a failure mode prediction")
	}
	if preds[0].Mode != "module_not_found" {
		t.Fatalf("top mode = %q, want module_not_found", preds[0].Mode)
	}
	if preds[0].Scope != "command+host" {
		t.Fatalf("scope = %q, want command+host", preds[0].Scope)
	}
	if preds[0].Failures != 3 || preds[0].Total != 4 {
		t.Fatalf("counts = %d/%d, want 3/4", preds[0].Failures, preds[0].Total)
	}
}
