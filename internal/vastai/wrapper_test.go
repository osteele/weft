package vastai

import (
	"strings"
	"testing"
)

func TestGenerateWrapper(t *testing.T) {
	wrapper := GenerateWrapper(42, "python train.py --epochs 10", "weft-results")

	// Check shebang
	if !strings.HasPrefix(wrapper, "#!/bin/bash\n") {
		t.Error("wrapper should start with bash shebang")
	}

	// Check job ID substitution
	if !strings.Contains(wrapper, `JOB_ID="42"`) {
		t.Error("wrapper should contain JOB_ID assignment")
	}

	// Check bucket
	if !strings.Contains(wrapper, `R2_BUCKET="weft-results"`) {
		t.Error("wrapper should contain R2_BUCKET assignment")
	}

	// Check command is embedded
	if !strings.Contains(wrapper, "python train.py --epochs 10") {
		t.Error("wrapper should contain the job command")
	}

	// Check R2 upload
	if !strings.Contains(wrapper, "rclone copy /tmp/results/") {
		t.Error("wrapper should upload results to R2")
	}

	// Check completion marker
	if !strings.Contains(wrapper, ".complete") {
		t.Error("wrapper should write completion marker")
	}

	// Check self-destruct
	if !strings.Contains(wrapper, "vastai destroy instance") {
		t.Error("wrapper should self-destruct")
	}

	// Check OOM debug info collection
	if !strings.Contains(wrapper, "dmesg") {
		t.Error("wrapper should capture dmesg on failure")
	}
	if !strings.Contains(wrapper, "nvidia-smi") {
		t.Error("wrapper should capture nvidia-smi on failure")
	}
}

func TestGenerateRcloneConfig(t *testing.T) {
	cfg := R2Config{
		AccountID:       "abc123",
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
		Bucket:          "my-bucket",
	}

	conf := GenerateRcloneConfig(cfg)

	if !strings.Contains(conf, "[r2]") {
		t.Error("should contain [r2] section")
	}
	if !strings.Contains(conf, "provider = Cloudflare") {
		t.Error("should specify Cloudflare provider")
	}
	if !strings.Contains(conf, "access_key_id = AKID") {
		t.Error("should contain access key")
	}
	if !strings.Contains(conf, "secret_access_key = SECRET") {
		t.Error("should contain secret key")
	}
	if !strings.Contains(conf, "abc123.r2.cloudflarestorage.com") {
		t.Error("should contain R2 endpoint with account ID")
	}
}

func TestGenerateWrapper_SpecialChars(t *testing.T) {
	// Test that commands with special characters are handled
	wrapper := GenerateWrapper(99, `echo "hello world" && python -c 'import sys; print(sys.argv)'`, "bucket")

	if !strings.Contains(wrapper, `echo "hello world"`) {
		t.Error("wrapper should preserve command with quotes")
	}
}
