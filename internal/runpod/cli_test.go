package runpod

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

func TestRunpodctlCommandEnvUsesExistingEnvKey(t *testing.T) {
	t.Setenv("RUNPOD_API_KEY", "from-env")
	env := runpodctlCommandEnv()
	if !envContainsKV(env, "RUNPOD_API_KEY", "from-env") {
		t.Fatalf("expected RUNPOD_API_KEY from environment, env=%v", env)
	}
}

func TestRunpodctlCommandEnvLoadsKeyFromConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("RUNPOD_API_KEY", "")
	runpodDir := filepath.Join(tmp, ".runpod")
	if err := os.MkdirAll(runpodDir, 0o755); err != nil {
		t.Fatalf("mkdir .runpod: %v", err)
	}
	configPath := filepath.Join(runpodDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[default]\napi_key = \"from-config\"\n"), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}

	env := runpodctlCommandEnv()
	if !envContainsKV(env, "RUNPOD_API_KEY", "from-config") {
		t.Fatalf("expected RUNPOD_API_KEY from ~/.runpod/config.toml, env=%v", env)
	}
	key, err := ReadAPIKey()
	if err != nil {
		t.Fatalf("ReadAPIKey: %v", err)
	}
	if key != "from-config" {
		t.Fatalf("ReadAPIKey = %q, want from-config", key)
	}
}

func TestCleanRunpodctlErrorDetailPrefersJSONError(t *testing.T) {
	stream := []byte(`{"error":"Something went wrong. Please try again later or contact support."}
Usage:
  runpodctl pod create [flags]

Flags:
  -h, --help   help for create
{"error":"failed to create pod: Something went wrong. Please try again later or contact support."}`)

	got := cleanRunpodctlErrorDetail(stream)
	want := "Something went wrong. Please try again later or contact support."
	if got != want {
		t.Fatalf("cleanRunpodctlErrorDetail = %q, want %q", got, want)
	}
}

func TestRunOutputTimeoutIsProviderCommandTimeout(t *testing.T) {
	runner := newCLIRunner("runpodctl")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := runner.runOutput(ctx, "/bin/sh", "-c", "sleep 1")
	if !errors.Is(err, cloud.ErrProviderCommandTimeout) {
		t.Fatalf("runOutput timeout err = %v, want ErrProviderCommandTimeout", err)
	}
	if strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("runOutput timeout err = %v, should not expose signal: killed", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("runOutput timeout err = %v, want timeout detail", err)
	}
}

func TestIsOfferUnavailableError_RunpodTransientCreateFailure(t *testing.T) {
	err := formatRunpodctlError(
		[]string{"pod", "create", "--gpu-id", "NVIDIA L40"},
		errTestExit{},
		[]byte(`{"error":"Something went wrong. Please try again later or contact support."}
Usage:
  runpodctl pod create [flags]`),
	)
	if !isOfferUnavailableError(err) {
		t.Fatalf("isOfferUnavailableError(%v) = false, want true", err)
	}
}

func TestIsOfferUnavailableError_RunpodMachineOutOfResources(t *testing.T) {
	err := formatRunpodctlError(
		[]string{"pod", "create", "--gpu-id", "NVIDIA GeForce RTX 4090"},
		errTestExit{},
		[]byte(`{"error":"This machine does not have the resources to deploy your pod. Please try a different machine"}
Usage:
  runpodctl pod create [flags]`),
	)
	if !isOfferUnavailableError(err) {
		t.Fatalf("isOfferUnavailableError(%v) = false, want true", err)
	}
}

type errTestExit struct{}

func (errTestExit) Error() string { return "exit status 1" }

func envContainsKV(env []string, key, value string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if strings.TrimPrefix(entry, prefix) == value {
				return true
			}
		}
	}
	return false
}
