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
	if opts.RunpodCloudType != cloud.RunpodCloudTypeCommunity {
		t.Fatalf("RunpodCloudType = %q, want %q", opts.RunpodCloudType, cloud.RunpodCloudTypeCommunity)
	}
}

func TestCloudCreateOpts_RunpodIncludesBootstrapTemplate(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runpod.BootstrapTemplateID = "tpl-bootstrap"
	opts, err := cfg.CloudCreateOpts(cloud.ProviderRunpod)
	if err != nil {
		t.Fatalf("CloudCreateOpts: %v", err)
	}
	if opts.TemplateID != "tpl-bootstrap" {
		t.Fatalf("TemplateID = %q, want tpl-bootstrap", opts.TemplateID)
	}
}

func TestCloudCreateOpts_RunpodHonorsCloudType(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runpod.CloudType = "secure"
	opts, err := cfg.CloudCreateOpts(cloud.ProviderRunpod)
	if err != nil {
		t.Fatalf("CloudCreateOpts: %v", err)
	}
	if opts.RunpodCloudType != cloud.RunpodCloudTypeSecure {
		t.Fatalf("RunpodCloudType = %q, want secure", opts.RunpodCloudType)
	}
}

func TestCloudCreateOpts_RunpodRejectsInvalidCloudType(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Runpod.CloudType = "private"
	if _, err := cfg.CloudCreateOpts(cloud.ProviderRunpod); err == nil {
		t.Fatal("CloudCreateOpts error = nil, want invalid cloud type error")
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
