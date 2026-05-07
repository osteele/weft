package runpod

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
)

type stubCLIResponse struct {
	out []byte
	err error
}

func newStubRunner(t *testing.T, combined map[string]stubCLIResponse, output map[string]stubCLIResponse) *cliRunner {
	t.Helper()
	path := "/opt/homebrew/bin/runpodctl"
	return &cliRunner{
		cliPath:  "runpodctl",
		lookPath: func(string) (string, error) { return path, nil },
		runCombined: func(_ context.Context, gotPath string, args ...string) ([]byte, error) {
			if gotPath != path {
				t.Fatalf("runCombined path = %q, want %q", gotPath, path)
			}
			key := strings.Join(args, " ")
			if resp, ok := combined[key]; ok {
				return resp.out, resp.err
			}
			t.Fatalf("unexpected combined command %q", key)
			return nil, nil
		},
		runOutput: func(_ context.Context, gotPath string, args ...string) ([]byte, error) {
			if gotPath != path {
				t.Fatalf("runOutput path = %q, want %q", gotPath, path)
			}
			key := strings.Join(args, " ")
			if resp, ok := output[key]; ok {
				return resp.out, resp.err
			}
			for prefix, resp := range output {
				if strings.HasSuffix(prefix, "*") && strings.HasPrefix(key, strings.TrimSuffix(prefix, "*")) {
					return resp.out, resp.err
				}
			}
			t.Fatalf("unexpected output command %q", key)
			return nil, nil
		},
	}
}

func TestDetectCapabilitiesModernCommands(t *testing.T) {
	runner := newStubRunner(t,
		map[string]stubCLIResponse{
			"version":         {out: []byte("runpodctl 2.1.6")},
			"get --help":      {out: []byte("Available Commands:\n  cloud\n  pod\n")},
			"gpu --help":      {out: []byte("Available Commands:\n  list\n")},
			"pod --help":      {out: []byte("Available Commands:\n  create\n  delete\n  get\n  list\n")},
			"template --help": {out: []byte("Available Commands:\n  create\n  get\n  list\n")},
		},
		nil,
	)

	caps, err := runner.detectCapabilities(context.Background())
	if err != nil {
		t.Fatalf("detectCapabilities: %v", err)
	}
	if got := joinCommand(caps.searchCommand); got != "gpu list" {
		t.Fatalf("searchCommand = %q", got)
	}
	if got := joinCommand(caps.podDeleteCommand); got != "pod delete" {
		t.Fatalf("podDeleteCommand = %q", got)
	}
	if got := joinCommand(caps.templateCreateCommand); got != "template create" {
		t.Fatalf("templateCreateCommand = %q", got)
	}
}

func TestDetectCapabilitiesFallsBackToLegacySearch(t *testing.T) {
	runner := newStubRunner(t,
		map[string]stubCLIResponse{
			"version":         {out: []byte("runpodctl 1.2.3")},
			"get --help":      {out: []byte("Available Commands:\n  gpu\n  pod\n")},
			"gpu --help":      {out: []byte("Available Commands:\n")},
			"pod --help":      {out: []byte("Available Commands:\n  get\n  list\n  delete\n")},
			"template --help": {out: []byte("Available Commands:\n  create\n  get\n  list\n")},
		},
		nil,
	)

	caps, err := runner.detectCapabilities(context.Background())
	if err != nil {
		t.Fatalf("detectCapabilities: %v", err)
	}
	if got := joinCommand(caps.searchCommand); got != "get gpu" {
		t.Fatalf("searchCommand = %q", got)
	}
}

func TestCreateInstanceWrapsUnavailableOffer(t *testing.T) {
	runner := newStubRunner(t,
		map[string]stubCLIResponse{
			"version":         {out: []byte("runpodctl 2.1.6")},
			"get --help":      {out: []byte("Available Commands:\n  cloud\n  pod\n")},
			"gpu --help":      {out: []byte("Available Commands:\n  list\n")},
			"pod --help":      {out: []byte("Available Commands:\n  create\n  delete\n  get\n  list\n")},
			"template --help": {out: []byte("Available Commands:\n  create\n  get\n  list\n")},
		},
		map[string]stubCLIResponse{
			"pod create --gpu-id RTX4090 --gpu-count 1 --template-id tpl-bootstrap": {err: errors.New("pod create: gpu RTX4090 no longer exists")},
		},
	)
	client := newCloudClientForTests(runner)
	_, err := client.CreateInstance("RTX4090", cloud.CreateOpts{TemplateID: "tpl-bootstrap"})
	if !errors.Is(err, cloud.ErrOfferUnavailable) {
		t.Fatalf("expected ErrOfferUnavailable, got %v", err)
	}
}

func TestParseSearchOutputFiltersGPUClass(t *testing.T) {
	input := []byte(`[
		{"id":"offer-4090","displayName":"NVIDIA GeForce RTX 4090","memoryInGb":24,"communityPrice":0.44,"maxGpuCount":1},
		{"id":"offer-2080","displayName":"NVIDIA GeForce RTX 2080 Ti","memoryInGb":11,"communityPrice":0.10,"maxGpuCount":1}
	]`)
	offers, err := parseSearchOutput(input, cloud.OfferConstraints{GPUClass: "ampere+", MinGPUMemGB: 24})
	if err != nil {
		t.Fatalf("parseSearchOutput: %v", err)
	}
	if len(offers) != 1 || offers[0].ProviderID != "offer-4090" {
		t.Fatalf("offers = %+v", offers)
	}
}

func TestTemplateMatchesSpec(t *testing.T) {
	spec := BootstrapTemplateSpec{
		Image:        "nvidia/cuda:12.4.1-runtime-ubuntu22.04",
		StartCommand: "bash -lc bash   /tmp/bootstrap.sh",
	}
	if !templateMatchesSpec(&TemplateInfo{
		Image:          spec.Image,
		DockerStartCmd: "bash,-lc,bash /tmp/bootstrap.sh",
	}, spec) {
		t.Fatal("expected template to match spec")
	}
}

func TestNormalizeStartCommand_RunpodCSV(t *testing.T) {
	if got := normalizeStartCommand("bash,-lc,echo hi"); got != "bash -lc echo hi" {
		t.Fatalf("normalizeStartCommand = %q", got)
	}
}

func TestSetupPersistsCompatibleDefaultImage(t *testing.T) {
	dir := t.TempDir()
	restore := config.SetConfigPathsForTesting(filepath.Join(dir, "config.toml"), filepath.Join(dir, "config.yaml"))
	defer restore()

	cfg := config.DefaultConfig()
	cfg.Runpod.Enabled = true
	cfg.Runpod.DefaultImage = "runpod/pytorch:test"

	runner := newStubRunner(t,
		map[string]stubCLIResponse{
			"version":         {out: []byte("runpodctl 2.1.6")},
			"get --help":      {out: []byte("Available Commands:\n  cloud\n  pod\n")},
			"gpu --help":      {out: []byte("Available Commands:\n  list\n")},
			"pod --help":      {out: []byte("Available Commands:\n  create\n  delete\n  get\n  list\n")},
			"template --help": {out: []byte("Available Commands:\n  create\n  get\n  list\n")},
		},
		map[string]stubCLIResponse{
			"user":                      {out: []byte(`{"id":"me"}`)},
			"template list --type user": {out: []byte(`[]`)},
			"template create --name *":  {out: []byte(`{"id":"tpl-created"}`)},
			"template get tpl-created":  {out: []byte(`{"id":"tpl-created","imageName":"runpod/pytorch:test","dockerStartCmd":"bash,-lc,placeholder"}`)},
		},
	)

	result, err := newManagerWithClient(newCloudClientForTests(runner)).Setup(cfg)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	data, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), `default_image = "runpod/pytorch:test"`) {
		t.Fatalf("config does not contain expected default image:\n%s", string(data))
	}
	if !strings.Contains(string(data), `bootstrap_template_id = "tpl-created"`) {
		t.Fatalf("config does not contain expected template id:\n%s", string(data))
	}
	if result.Diagnosis == nil {
		t.Fatal("expected diagnosis")
	}
	if !result.Diagnosis.SearchReady {
		t.Fatalf("expected search-ready diagnosis, got %+v", result.Diagnosis)
	}
	if len(result.Diagnosis.LaunchChecks) < 2 || !result.Diagnosis.LaunchChecks[1].OK {
		t.Fatalf("expected runpod image launch check to pass, got %+v", result.Diagnosis.LaunchChecks)
	}
}

func TestSetupNormalizesIncompatibleDefaultImage(t *testing.T) {
	dir := t.TempDir()
	restore := config.SetConfigPathsForTesting(filepath.Join(dir, "config.toml"), filepath.Join(dir, "config.yaml"))
	defer restore()

	cfg := config.DefaultConfig()
	cfg.Runpod.Enabled = true
	cfg.Runpod.DefaultImage = "pytorch/pytorch:2.6.0-cuda12.4-cudnn9-runtime"

	runner := newStubRunner(t,
		map[string]stubCLIResponse{
			"version":         {out: []byte("runpodctl 2.1.6")},
			"get --help":      {out: []byte("Available Commands:\n  cloud\n  pod\n")},
			"gpu --help":      {out: []byte("Available Commands:\n  list\n")},
			"pod --help":      {out: []byte("Available Commands:\n  create\n  delete\n  get\n  list\n")},
			"template --help": {out: []byte("Available Commands:\n  create\n  get\n  list\n")},
		},
		map[string]stubCLIResponse{
			"user":                      {out: []byte(`{"id":"me"}`)},
			"template list --type user": {out: []byte(`[]`)},
			"template create --name *":  {out: []byte(`{"id":"tpl-created"}`)},
			"template get tpl-created":  {out: []byte(`{"id":"tpl-created","imageName":"runpod/base:1.0.2-ubuntu2204","dockerStartCmd":"bash,-lc,placeholder"}`)},
		},
	)

	result, err := newManagerWithClient(newCloudClientForTests(runner)).Setup(cfg)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	data, err := os.ReadFile(config.ConfigPath())
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), `default_image = "`+cloud.DefaultRunpodImage+`"`) {
		t.Fatalf("config does not contain normalized default image:\n%s", string(data))
	}
	if !strings.Contains(string(data), `bootstrap_template_id = "tpl-created"`) {
		t.Fatalf("config does not contain expected template id:\n%s", string(data))
	}
	if result.Diagnosis == nil {
		t.Fatal("expected diagnosis")
	}
	if !result.Diagnosis.SearchReady {
		t.Fatalf("expected search-ready diagnosis, got %+v", result.Diagnosis)
	}
	if len(result.Diagnosis.LaunchChecks) < 2 || !result.Diagnosis.LaunchChecks[1].OK {
		t.Fatalf("expected runpod image launch check to pass, got %+v", result.Diagnosis.LaunchChecks)
	}
}
