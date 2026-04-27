package hostsync

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/inventory"
)

func TestEnsureQueueRunnerStartedSurfacesAgentDeployFailure(t *testing.T) {
	originalFindHostSpec := findHostSpecFunc
	originalEnsureAgentUpToDate := ensureAgentUpToDateFunc
	originalLoadConfig := loadConfigFunc
	originalGetSlackWebhook := getSlackWebhookFunc
	originalDeployNotifyScript := deployNotifyScriptFunc
	originalBuildRunnerEnvPrefix := buildRunnerEnvPrefixFunc
	originalEnsureRunnerStarted := ensureRunnerStartedFunc
	t.Cleanup(func() {
		findHostSpecFunc = originalFindHostSpec
		ensureAgentUpToDateFunc = originalEnsureAgentUpToDate
		loadConfigFunc = originalLoadConfig
		getSlackWebhookFunc = originalGetSlackWebhook
		deployNotifyScriptFunc = originalDeployNotifyScript
		buildRunnerEnvPrefixFunc = originalBuildRunnerEnvPrefix
		ensureRunnerStartedFunc = originalEnsureRunnerStarted
	})

	findHostSpecFunc = func(host string) *inventory.HostSpec {
		return &inventory.HostSpec{Name: host, OS: "linux", Arch: "amd64"}
	}
	loadConfigFunc = func() (*config.Config, error) { return &config.Config{}, nil }
	getSlackWebhookFunc = func() string { return "" }
	deployNotifyScriptFunc = func(host, webhook string) {}
	buildRunnerEnvPrefixFunc = func(webhook string) string { return "" }
	ensureRunnerStartedFunc = func(host, envPrefix, r2Bucket string, setupTimeout time.Duration) (bool, error) {
		t.Fatal("runner should not start when agent deploy fails")
		return false, nil
	}
	ensureAgentUpToDateFunc = func(host string, spec inventory.HostSpec, opts agentdeploy.EnsureAgentOptions) (bool, error) {
		return false, os.ErrPermission
	}

	started, err := EnsureQueueRunnerStarted("studio")
	if err == nil {
		t.Fatal("expected error")
	}
	if started {
		t.Fatal("started = true, want false")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected os.ErrPermission in error chain, got: %v", err)
	}
}

func TestEnsureQueueRunnerStartedPassesConfiguredR2Bucket(t *testing.T) {
	originalFindHostSpec := findHostSpecFunc
	originalEnsureAgentUpToDate := ensureAgentUpToDateFunc
	originalEnsureRcloneConfig := ensureRcloneConfigFunc
	originalLoadConfig := loadConfigFunc
	originalGetSlackWebhook := getSlackWebhookFunc
	originalDeployNotifyScript := deployNotifyScriptFunc
	originalBuildRunnerEnvPrefix := buildRunnerEnvPrefixFunc
	originalEnsureRunnerStarted := ensureRunnerStartedFunc
	t.Cleanup(func() {
		findHostSpecFunc = originalFindHostSpec
		ensureAgentUpToDateFunc = originalEnsureAgentUpToDate
		ensureRcloneConfigFunc = originalEnsureRcloneConfig
		loadConfigFunc = originalLoadConfig
		getSlackWebhookFunc = originalGetSlackWebhook
		deployNotifyScriptFunc = originalDeployNotifyScript
		buildRunnerEnvPrefixFunc = originalBuildRunnerEnvPrefix
		ensureRunnerStartedFunc = originalEnsureRunnerStarted
	})

	findHostSpecFunc = func(host string) *inventory.HostSpec {
		return &inventory.HostSpec{Name: host, OS: "linux", Arch: "amd64"}
	}
	ensureAgentUpToDateFunc = func(host string, spec inventory.HostSpec, opts agentdeploy.EnsureAgentOptions) (bool, error) {
		return false, nil
	}
	loadConfigFunc = func() (*config.Config, error) {
		return &config.Config{
			Vastai: config.VastaiConfig{
				R2: config.R2Config{
					Bucket:          "test-bucket",
					AccountID:       "account",
					AccessKeyID:     "key",
					SecretAccessKey: "secret",
				},
			},
		}, nil
	}
	getSlackWebhookFunc = func() string { return "" }
	deployNotifyScriptFunc = func(host, webhook string) {}
	buildRunnerEnvPrefixFunc = func(webhook string) string { return "WEBHOOK=1 " }

	rcloneCalled := false
	ensureRcloneConfigFunc = func(host string, cfg cloud.R2Config) error {
		rcloneCalled = true
		if cfg.Bucket != "test-bucket" {
			t.Fatalf("unexpected bucket %q", cfg.Bucket)
		}
		return nil
	}

	ensureRunnerStartedFunc = func(host, envPrefix, r2Bucket string, setupTimeout time.Duration) (bool, error) {
		if host != "studio" {
			t.Fatalf("host = %q, want studio", host)
		}
		if envPrefix != "WEBHOOK=1 " {
			t.Fatalf("envPrefix = %q, want WEBHOOK=1 ", envPrefix)
		}
		if r2Bucket != "test-bucket" {
			t.Fatalf("r2Bucket = %q, want test-bucket", r2Bucket)
		}
		return true, nil
	}

	started, err := EnsureQueueRunnerStarted("studio")
	if err != nil {
		t.Fatalf("EnsureQueueRunnerStarted: %v", err)
	}
	if !started {
		t.Fatal("started = false, want true")
	}
	if !rcloneCalled {
		t.Fatal("expected rclone config deployment")
	}
}
