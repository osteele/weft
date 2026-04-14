package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestBuildCreatePodArgs_WithTemplateAndEnv(t *testing.T) {
	args, err := buildCreatePodArgs("NVIDIA A100 80GB PCIe", cloud.CreateOpts{
		TemplateID: "tpl-bootstrap",
		GPUCount:   4,
		DiskGB:     120,
		Label:      "weft/c42",
		EnvVars: map[string]string{
			"R2_BUCKET":                "bucket",
			cloud.R2BootstrapKeyEnvVar: "bootstrap/42.sh",
		},
	})
	if err != nil {
		t.Fatalf("buildCreatePodArgs: %v", err)
	}

	got := strings.Join(args, " ")
	for _, want := range []string{
		"pod create",
		"--gpu-id NVIDIA A100 80GB PCIe",
		"--gpu-count 4",
		"--template-id tpl-bootstrap",
		"--volume-in-gb 120",
		"--name weft/c42",
		"--env",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("args %q missing %q", got, want)
		}
	}
}

func TestBuildCreatePodArgs_RejectsOnStartWithoutTemplate(t *testing.T) {
	_, err := buildCreatePodArgs("RTX 4090", cloud.CreateOpts{
		Image:      "ubuntu:22.04",
		OnStartCmd: "echo hello",
	})
	if err == nil {
		t.Fatal("expected error for per-pod startup command")
	}
	if !errors.Is(err, ErrPerPodStartupUnsupported) {
		t.Fatalf("error = %v, want ErrPerPodStartupUnsupported", err)
	}
}

func TestParseCreatedPodID_JSON(t *testing.T) {
	data, err := json.Marshal(Pod{ID: "pod-123"})
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	id, err := parseCreatedPodID(data)
	if err != nil {
		t.Fatalf("parseCreatedPodID: %v", err)
	}
	if id != "pod-123" {
		t.Fatalf("id = %q, want %q", id, "pod-123")
	}
}

func TestParseGPUTypeOutput(t *testing.T) {
	input := `[
		{
			"id": "NVIDIA GeForce RTX 4090",
			"displayName": "RTX 4090",
			"memoryInGb": 24,
			"secureCloud": true,
			"communityCloud": true,
			"securePrice": 0.74,
			"communityPrice": 0.44,
			"maxGpuCount": 1
		},
		{
			"id": "NVIDIA A100 80GB PCIe",
			"displayName": "A100 80GB",
			"memoryInGb": 80,
			"secureCloud": true,
			"communityCloud": true,
			"securePrice": 1.99,
			"communityPrice": 1.64,
			"maxGpuCount": 8
		},
		{
			"id": "NVIDIA GeForce GTX 1080",
			"displayName": "GTX 1080",
			"memoryInGb": 8,
			"secureCloud": false,
			"communityCloud": true,
			"securePrice": 0,
			"communityPrice": 0.10,
			"maxGpuCount": 1
		}
	]`

	t.Run("no constraints", func(t *testing.T) {
		offers, err := parseGPUTypeOutput([]byte(input), cloud.OfferConstraints{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(offers) != 3 {
			t.Fatalf("expected 3 offers, got %d", len(offers))
		}
		// Check community price is used
		if offers[0].CostPerHour != 0.44 {
			t.Errorf("expected community price 0.44, got %f", offers[0].CostPerHour)
		}
		if offers[0].Provider != cloud.ProviderRunpod {
			t.Errorf("expected runpod provider, got %v", offers[0].Provider)
		}
	})

	t.Run("min memory filter", func(t *testing.T) {
		offers, err := parseGPUTypeOutput([]byte(input), cloud.OfferConstraints{MinGPUMemGB: 24})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(offers) != 2 {
			t.Fatalf("expected 2 offers (24GB+), got %d", len(offers))
		}
	})

	t.Run("high memory filter", func(t *testing.T) {
		offers, err := parseGPUTypeOutput([]byte(input), cloud.OfferConstraints{MinGPUMemGB: 80})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(offers) != 1 {
			t.Fatalf("expected 1 offer (80GB+), got %d", len(offers))
		}
		if offers[0].GPUName != "A100 80GB" {
			t.Errorf("expected A100, got %s", offers[0].GPUName)
		}
	})
}

func TestSearchOffersCachesCapabilities(t *testing.T) {
	const path = "/opt/homebrew/bin/runpodctl"

	var (
		mu     sync.Mutex
		counts = make(map[string]int)
	)
	record := func(key string) {
		mu.Lock()
		counts[key]++
		mu.Unlock()
	}

	runner := &cliRunner{
		cliPath:  "runpodctl",
		lookPath: func(string) (string, error) { return path, nil },
		runCombined: func(_ context.Context, gotPath string, args ...string) ([]byte, error) {
			if gotPath != path {
				t.Fatalf("runCombined path = %q, want %q", gotPath, path)
			}
			key := strings.Join(args, " ")
			record(key)
			switch key {
			case "version":
				return []byte("runpodctl 2.1.6"), nil
			case "get --help":
				return []byte("Available Commands:\n  cloud\n  pod\n"), nil
			case "gpu --help":
				return []byte("Available Commands:\n  list\n"), nil
			case "pod --help":
				return []byte("Available Commands:\n  create\n  delete\n  get\n  list\n"), nil
			case "template --help":
				return []byte("Available Commands:\n  create\n  get\n  list\n"), nil
			default:
				t.Fatalf("unexpected combined command %q", key)
				return nil, nil
			}
		},
		runOutput: func(_ context.Context, gotPath string, args ...string) ([]byte, error) {
			if gotPath != path {
				t.Fatalf("runOutput path = %q, want %q", gotPath, path)
			}
			key := strings.Join(args, " ")
			record(key)
			if key != "gpu list" {
				t.Fatalf("unexpected output command %q", key)
			}
			return []byte(`[
				{"id":"offer-4090","displayName":"RTX 4090","memoryInGb":24,"communityPrice":0.44,"maxGpuCount":1}
			]`), nil
		},
	}

	client := newCloudClientForTests(runner)
	for range 2 {
		offers, err := client.SearchOffers(cloud.OfferConstraints{})
		if err != nil {
			t.Fatalf("SearchOffers: %v", err)
		}
		if len(offers) != 1 || offers[0].ProviderID != "offer-4090" {
			t.Fatalf("offers = %+v", offers)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{"version", "get --help", "gpu --help", "pod --help", "template --help"} {
		if counts[key] != 1 {
			t.Fatalf("%s count = %d, want 1", key, counts[key])
		}
	}
	if counts["gpu list"] != 2 {
		t.Fatalf("gpu list count = %d, want 2", counts["gpu list"])
	}
}

func TestParseSearchOutput_AllowsCLIWarningPrefix(t *testing.T) {
	input := []byte(`warning: 'runpodctl get cloud' is deprecated
[{"id":"offer-4090","displayName":"RTX 4090","memoryInGb":24,"communityPrice":0.44,"maxGpuCount":1}]`)

	offers, err := parseSearchOutput(input, cloud.OfferConstraints{})
	if err != nil {
		t.Fatalf("parseSearchOutput: %v", err)
	}
	if len(offers) != 1 || offers[0].ProviderID != "offer-4090" {
		t.Fatalf("offers = %+v", offers)
	}
}
