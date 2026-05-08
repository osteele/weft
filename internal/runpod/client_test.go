package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

func TestEncodeTemplateStartCommandArg(t *testing.T) {
	t.Run("encodes bash -lc command", func(t *testing.T) {
		got, err := encodeTemplateStartCommandArg("bash -lc echo hello")
		if err != nil {
			t.Fatalf("encodeTemplateStartCommandArg: %v", err)
		}
		if got != "bash,-lc,echo hello" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("rejects missing prefix", func(t *testing.T) {
		_, err := encodeTemplateStartCommandArg("echo hello")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("rejects commas in payload", func(t *testing.T) {
		_, err := encodeTemplateStartCommandArg("bash -lc echo a,b")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestBuildCreatePodArgs_WithTemplateAndEnv(t *testing.T) {
	args, err := buildCreatePodArgs("NVIDIA A100 80GB PCIe", cloud.CreateOpts{
		TemplateID: "tpl-bootstrap",
		Image:      "runpod/pytorch:stable",
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
		"--image runpod/pytorch:stable",
		"--volume-in-gb 120",
		"--name weft/c42",
		"--env",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("args %q missing %q", got, want)
		}
	}
}

func TestBuildCreatePodArgs_DefaultRunpodImageUsesOfficialTemplate(t *testing.T) {
	args, err := buildCreatePodArgs("NVIDIA L4", cloud.CreateOpts{
		Image: cloud.DefaultRunpodImage,
	})
	if err != nil {
		t.Fatalf("buildCreatePodArgs: %v", err)
	}
	got := strings.Join(args, " ")
	if strings.Contains(got, "--image") {
		t.Fatalf("args %q unexpectedly contain --image", got)
	}
	if !strings.Contains(got, "--template-id "+officialRunpodUbuntuTemplateID) {
		t.Fatalf("args %q missing official template", got)
	}
}

func TestBuildCreatePodArgs_NonDefaultImageUsesImageFlag(t *testing.T) {
	const image = "runpod/pytorch:stable"
	args, err := buildCreatePodArgs("NVIDIA L4", cloud.CreateOpts{
		Image:            image,
		MinCUDAVersion:   "12.8",
		RunpodRegistryID: "reg-123",
	})
	if err != nil {
		t.Fatalf("buildCreatePodArgs: %v", err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--image "+image) {
		t.Fatalf("args %q missing --image %q", got, image)
	}
	if strings.Contains(got, "--template-id "+officialRunpodUbuntuTemplateID) {
		t.Fatalf("args %q unexpectedly contain official template", got)
	}
	if strings.Contains(got, "--min-cuda-version") {
		t.Fatalf("args %q unexpectedly contain unsupported min CUDA flag", got)
	}
	if !strings.Contains(got, "--registry-auth-id reg-123") {
		t.Fatalf("args %q missing registry auth ID", got)
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
	prev := fetchGPUTypesFunc
	t.Cleanup(func() { fetchGPUTypesFunc = prev })
	calls := 0
	fetchGPUTypesFunc = func(context.Context) ([]gqlGPUType, error) {
		calls++
		price := 0.44
		stock := "High"
		return []gqlGPUType{
			{
				ID:          "offer-4090",
				DisplayName: "RTX 4090",
				MemoryInGb:  24,
				SecureCloud: true,
				LowestPrice: &struct {
					MinimumBidPrice      *float64 `json:"minimumBidPrice"`
					UninterruptablePrice *float64 `json:"uninterruptablePrice"`
					StockStatus          *string  `json:"stockStatus"`
				}{
					UninterruptablePrice: &price,
					StockStatus:          &stock,
				},
			},
		}, nil
	}

	client := newCloudClientForTests(newCLIRunner("runpodctl"))
	for range 2 {
		offers, err := client.SearchOffers(cloud.OfferConstraints{})
		if err != nil {
			t.Fatalf("SearchOffers: %v", err)
		}
		if len(offers) != 1 || offers[0].ProviderID != "offer-4090" {
			t.Fatalf("offers = %+v", offers)
		}
	}
	if calls != 2 {
		t.Fatalf("fetchGPUTypes calls = %d, want 2", calls)
	}
}

func TestShowUserParsesBalance(t *testing.T) {
	const path = "/opt/homebrew/bin/runpodctl"
	runner := &cliRunner{
		cliPath:  "runpodctl",
		lookPath: func(string) (string, error) { return path, nil },
		runCombined: func(_ context.Context, gotPath string, args ...string) ([]byte, error) {
			if gotPath != path {
				t.Fatalf("runCombined path = %q, want %q", gotPath, path)
			}
			switch strings.Join(args, " ") {
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
				t.Fatalf("unexpected combined command %q", strings.Join(args, " "))
				return nil, nil
			}
		},
		runOutput: func(_ context.Context, gotPath string, args ...string) ([]byte, error) {
			if gotPath != path {
				t.Fatalf("runOutput path = %q, want %q", gotPath, path)
			}
			if strings.Join(args, " ") == "user" {
				return []byte(`{"clientBalance":12.5,"currentSpendPerHr":0.42,"spendLimit":80,"notifyLowBalance":true}`), nil
			}
			t.Fatalf("unexpected output command %q", strings.Join(args, " "))
			return nil, nil
		},
	}

	user, err := newCloudClientForTests(runner).ShowUser()
	if err != nil {
		t.Fatalf("ShowUser: %v", err)
	}
	if user.ClientBalance != 12.5 {
		t.Fatalf("ClientBalance = %v, want 12.5", user.ClientBalance)
	}
	if user.CurrentSpendHr != 0.42 {
		t.Fatalf("CurrentSpendHr = %v, want 0.42", user.CurrentSpendHr)
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

func TestParseSSHInfoCommand(t *testing.T) {
	t.Run("json command", func(t *testing.T) {
		got, err := parseSSHInfoCommand([]byte(`{"command":"ssh -i /tmp/runpod_key -p 22000 root@1.2.3.4"}`))
		if err != nil {
			t.Fatalf("parseSSHInfoCommand: %v", err)
		}
		if got != "ssh -i /tmp/runpod_key -p 22000 root@1.2.3.4" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("json snake_case command", func(t *testing.T) {
		got, err := parseSSHInfoCommand([]byte(`{"ssh_command":"ssh -i /tmp/runpod_key -p 22000 root@1.2.3.4"}`))
		if err != nil {
			t.Fatalf("parseSSHInfoCommand: %v", err)
		}
		if got != "ssh -i /tmp/runpod_key -p 22000 root@1.2.3.4" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("json error", func(t *testing.T) {
		_, err := parseSSHInfoCommand([]byte(`{"error":"pod not ready"}`))
		if err == nil {
			t.Fatal("expected error")
		}
		if !isPodNotReadyError(err) {
			t.Fatalf("error should be not-ready, got %v", err)
		}
	})

	t.Run("text fallback", func(t *testing.T) {
		got, err := parseSSHInfoCommand([]byte("Use this command:\nssh -i /tmp/key -p 22000 root@8.8.8.8\n"))
		if err != nil {
			t.Fatalf("parseSSHInfoCommand: %v", err)
		}
		if got != "ssh -i /tmp/key -p 22000 root@8.8.8.8" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestInjectNonInteractiveSSHOptions(t *testing.T) {
	t.Cleanup(func() { cloud.SetSSHIdentityFile("") })
	cloud.SetSSHIdentityFile("")
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "typical runpodctl command",
			in:   "ssh -i /tmp/runpod_key -p 22000 root@1.2.3.4",
			want: "ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -o IdentityAgent=none -i /tmp/runpod_key -p 22000 root@1.2.3.4",
		},
		{
			name: "leading whitespace",
			in:   "  ssh root@host",
			want: "ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -o IdentityAgent=none root@host",
		},
		{
			name: "does not duplicate existing identities option",
			in:   "ssh -o IdentitiesOnly=yes -i /tmp/runpod_key -p 22000 root@1.2.3.4",
			want: "ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentityAgent=none -o IdentitiesOnly=yes -i /tmp/runpod_key -p 22000 root@1.2.3.4",
		},
		{
			name: "recognizes split option form",
			in:   "ssh -o IdentitiesOnly=yes -o BatchMode=yes -i /tmp/runpod_key root@host",
			want: "ssh -F /dev/null -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentityAgent=none -o IdentitiesOnly=yes -o BatchMode=yes -i /tmp/runpod_key root@host",
		},
		{
			name: "non-ssh command left alone",
			in:   "scp file root@host:/tmp/",
			want: "scp file root@host:/tmp/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := injectNonInteractiveSSHOptions(tc.in)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInjectNonInteractiveSSHOptions_UsesConfiguredIdentity(t *testing.T) {
	t.Cleanup(func() { cloud.SetSSHIdentityFile("") })
	cloud.SetSSHIdentityFile("/tmp/weft_cloud_ed25519")

	got := injectNonInteractiveSSHOptions("ssh root@host")
	want := "ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -o IdentityAgent=none -i '/tmp/weft_cloud_ed25519' root@host"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestInjectNonInteractiveSSHOptions_ConfiguredIdentityReplacesRunpodIdentity(t *testing.T) {
	t.Cleanup(func() { cloud.SetSSHIdentityFile("") })
	cloud.SetSSHIdentityFile("/tmp/weft_cloud_ed25519")

	got := injectNonInteractiveSSHOptions("ssh -i /tmp/RunPod-Key-Go -p 22000 root@1.2.3.4")
	want := "ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes -o IdentityAgent=none -i '/tmp/weft_cloud_ed25519' -p 22000 root@1.2.3.4"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWaitForSSHCommand_RespectsContextDeadline(t *testing.T) {
	const path = "/opt/homebrew/bin/runpodctl"
	runner := &cliRunner{
		cliPath:  "runpodctl",
		lookPath: func(string) (string, error) { return path, nil },
		runCombined: func(_ context.Context, gotPath string, args ...string) ([]byte, error) {
			if gotPath != path {
				t.Fatalf("runCombined path = %q, want %q", gotPath, path)
			}
			switch strings.Join(args, " ") {
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
				t.Fatalf("unexpected combined command %q", strings.Join(args, " "))
				return nil, nil
			}
		},
		runOutput: func(_ context.Context, gotPath string, args ...string) ([]byte, error) {
			if gotPath != path {
				t.Fatalf("runOutput path = %q, want %q", gotPath, path)
			}
			if strings.Join(args, " ") == "ssh info pod-123" {
				return nil, errors.New("runpodctl ssh info pod-123: pod not ready")
			}
			t.Fatalf("unexpected output command %q", strings.Join(args, " "))
			return nil, nil
		},
	}

	client := newCloudClientForTests(runner)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.waitForSSHCommand(ctx, "pod-123")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("waitForSSHCommand took too long after deadline: %s", elapsed)
	}
}
