package config

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestCloudCreateOpts_RunpodDefaults(t *testing.T) {
	cfg := DefaultConfig()
	opts, err := cfg.CloudCreateOpts(cloud.ProviderRunpod)
	if err != nil {
		t.Fatalf("CloudCreateOpts: %v", err)
	}
	if opts.Image != cloud.DefaultRunpodImage {
		t.Fatalf("image = %q, want %q", opts.Image, cloud.DefaultRunpodImage)
	}
	if opts.TemplateID != "" {
		t.Fatalf("template_id = %q, want empty", opts.TemplateID)
	}
	if !opts.SSHEnabled {
		t.Fatal("SSHEnabled = false, want true")
	}
}

func TestCloudCreateOpts_IncludesCloudSSHIdentity(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Cloud.SSH.IdentityFile = "~/.ssh/weft_cloud_ed25519"

	opts, err := cfg.CloudCreateOpts(cloud.ProviderVastai)
	if err != nil {
		t.Fatalf("CloudCreateOpts: %v", err)
	}
	if opts.SSHIdentityFile == "" {
		t.Fatal("SSHIdentityFile is empty")
	}
	if opts.SSHPublicKeyFile != opts.SSHIdentityFile+".pub" {
		t.Fatalf("SSHPublicKeyFile = %q, want %q", opts.SSHPublicKeyFile, opts.SSHIdentityFile+".pub")
	}
}

func TestCloudCreateOpts_RunpodIgnoresIncompatibleConfiguredImage(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runpod.DefaultImage = "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime"
	opts, err := cfg.CloudCreateOpts(cloud.ProviderRunpod)
	if err != nil {
		t.Fatalf("CloudCreateOpts: %v", err)
	}
	if opts.Image != cloud.DefaultRunpodImage {
		t.Fatalf("image = %q, want %q", opts.Image, cloud.DefaultRunpodImage)
	}
}

func TestCloudCreateOpts_RunpodHonorsCompatibleConfiguredImage(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runpod.DefaultImage = "runpod/pytorch:stable"
	opts, err := cfg.CloudCreateOpts(cloud.ProviderRunpod)
	if err != nil {
		t.Fatalf("CloudCreateOpts: %v", err)
	}
	if opts.Image != "runpod/pytorch:stable" {
		t.Fatalf("image = %q", opts.Image)
	}
}
