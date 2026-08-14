package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

func TestFetchGPUTypesRejectsGraphQLHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"message":"invalid API key"}]}`))
	}))
	defer server.Close()

	oldEndpoint, oldClient := graphqlEndpointURL, graphqlHTTPClient
	t.Cleanup(func() {
		graphqlEndpointURL = oldEndpoint
		graphqlHTTPClient = oldClient
	})
	graphqlEndpointURL = server.URL
	graphqlHTTPClient = server.Client()
	t.Setenv("RUNPOD_API_KEY", "test-key")

	_, err := fetchGPUTypes(context.Background(), cloud.RunpodCloudTypeCommunity)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "invalid API key") {
		t.Fatalf("fetchGPUTypes error = %v, want bounded HTTP/API error", err)
	}
}

func TestFetchGPUTypesRejectsMissingGraphQLData(t *testing.T) {
	for _, payload := range []string{"null", "{}", `{"data":null}`} {
		t.Run(payload, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(payload))
			}))
			defer server.Close()

			oldEndpoint, oldClient := graphqlEndpointURL, graphqlHTTPClient
			t.Cleanup(func() {
				graphqlEndpointURL = oldEndpoint
				graphqlHTTPClient = oldClient
			})
			graphqlEndpointURL = server.URL
			graphqlHTTPClient = server.Client()
			t.Setenv("RUNPOD_API_KEY", "test-key")

			if _, err := fetchGPUTypes(context.Background(), cloud.RunpodCloudTypeCommunity); err == nil {
				t.Fatalf("fetchGPUTypes accepted malformed response %s", payload)
			}
		})
	}
}

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

func TestSelfDestructCmdDoesNotMaskDeleteFailure(t *testing.T) {
	client := NewCloudClient()
	cmd := client.SelfDestructCmd("pod-123")
	if strings.Contains(cmd, "|| true") {
		t.Fatalf("SelfDestructCmd masks delete failures: %q", cmd)
	}
	if strings.Contains(cmd, "2>/dev/null") {
		t.Fatalf("SelfDestructCmd hides delete stderr: %q", cmd)
	}
	if !strings.Contains(cmd, "runpodctl pod delete") {
		t.Fatalf("SelfDestructCmd = %q, want runpodctl pod delete", cmd)
	}
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
		"--cloud-type COMMUNITY",
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

func TestBuildCreatePodArgs_SecureCloudType(t *testing.T) {
	args, err := buildCreatePodArgs("NVIDIA L4", cloud.CreateOpts{
		Image:           cloud.DefaultRunpodImage,
		RunpodCloudType: cloud.RunpodCloudTypeSecure,
	})
	if err != nil {
		t.Fatalf("buildCreatePodArgs: %v", err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--cloud-type SECURE") {
		t.Fatalf("args %q missing secure cloud type", got)
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

func TestParsePodRejectsNullOrMissingIdentity(t *testing.T) {
	for _, input := range []string{"null", `{ "desiredStatus": "RUNNING" }`} {
		if _, err := parsePod([]byte(input)); err == nil {
			t.Fatalf("parsePod(%q) returned nil error", input)
		}
	}
}

func TestParsePodsRejectsMissingIdentity(t *testing.T) {
	if _, err := parsePods([]byte(`[{"desiredStatus":"RUNNING"}]`)); err == nil {
		t.Fatal("parsePods accepted a row without a provider ID")
	}
}

func TestParsePodsRejectsEmptyOrNullResponse(t *testing.T) {
	for _, payload := range []string{"", "null"} {
		if _, err := parsePods([]byte(payload)); err == nil {
			t.Fatalf("parsePods accepted malformed response %q", payload)
		}
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
			"stockStatus": "Medium",
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
			"stockStatus": "High",
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
			"stockStatus": "Low",
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
		if offers[0].StockStatus != "Medium" {
			t.Errorf("expected stock status Medium, got %q", offers[0].StockStatus)
		}
	})

	t.Run("secure cloud uses secure price and filters unsupported", func(t *testing.T) {
		offers, err := parseGPUTypeOutput([]byte(input), cloud.OfferConstraints{RunpodCloudType: cloud.RunpodCloudTypeSecure})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(offers) != 2 {
			t.Fatalf("expected 2 secure offers, got %d", len(offers))
		}
		if offers[0].CostPerHour != 0.74 {
			t.Errorf("expected secure price 0.74, got %f", offers[0].CostPerHour)
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

func TestBuildOffersFromGraphQLExactL4DoesNotMatchL40(t *testing.T) {
	price := 0.44
	stock := "High"
	gpuTypes := []gqlGPUType{
		{
			ID:             "NVIDIA L4",
			DisplayName:    "NVIDIA L4",
			MemoryInGb:     24,
			CommunityCloud: true,
			LowestPrice: &gqlLowestPrice{
				UninterruptablePrice: &price,
				StockStatus:          &stock,
			},
		},
		{
			ID:             "NVIDIA L40",
			DisplayName:    "NVIDIA L40",
			MemoryInGb:     48,
			CommunityCloud: true,
			LowestPrice: &gqlLowestPrice{
				UninterruptablePrice: &price,
				StockStatus:          &stock,
			},
		},
		{
			ID:             "NVIDIA L40S",
			DisplayName:    "NVIDIA L40S",
			MemoryInGb:     48,
			CommunityCloud: true,
			LowestPrice: &gqlLowestPrice{
				UninterruptablePrice: &price,
				StockStatus:          &stock,
			},
		},
	}

	offers, err := buildOffersFromGraphQL(gpuTypes, cloud.OfferConstraints{GPUClass: "l4", MinGPUMemGB: 20})
	if err != nil {
		t.Fatalf("buildOffersFromGraphQL: %v", err)
	}
	if len(offers) != 1 || offers[0].ProviderID != "NVIDIA L4" {
		t.Fatalf("offers = %+v, want only NVIDIA L4", offers)
	}
	if offers[0].StockStatus != "High" {
		t.Fatalf("StockStatus = %q, want High", offers[0].StockStatus)
	}
}

func TestBuildOffersFromGraphQLRejectsMissingIdentity(t *testing.T) {
	price := 1.0
	stock := "High"
	_, err := buildOffersFromGraphQL([]gqlGPUType{{
		LowestPrice:    &gqlLowestPrice{UninterruptablePrice: &price, StockStatus: &stock},
		MemoryInGb:     24,
		CommunityCloud: true,
	}}, cloud.OfferConstraints{})
	if err == nil {
		t.Fatal("buildOffersFromGraphQL accepted GPU type without an ID")
	}
}

func TestSearchOffersCachesCapabilities(t *testing.T) {
	prev := fetchGPUTypesFunc
	t.Cleanup(func() { fetchGPUTypesFunc = prev })
	calls := 0
	fetchGPUTypesFunc = func(_ context.Context, cloudType string) ([]gqlGPUType, error) {
		calls++
		if cloudType != cloud.RunpodCloudTypeCommunity {
			t.Fatalf("cloudType = %q, want community", cloudType)
		}
		price := 0.44
		stock := "High"
		return []gqlGPUType{
			{
				ID:             "offer-4090",
				DisplayName:    "RTX 4090",
				MemoryInGb:     24,
				SecureCloud:    true,
				CommunityCloud: true,
				LowestPrice: &gqlLowestPrice{
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

func TestSearchOffersUsesClientCloudType(t *testing.T) {
	prev := fetchGPUTypesFunc
	t.Cleanup(func() { fetchGPUTypesFunc = prev })
	fetchGPUTypesFunc = func(_ context.Context, cloudType string) ([]gqlGPUType, error) {
		if cloudType != cloud.RunpodCloudTypeSecure {
			t.Fatalf("cloudType = %q, want secure", cloudType)
		}
		price := 0.74
		stock := "High"
		return []gqlGPUType{
			{
				ID:          "offer-4090",
				DisplayName: "RTX 4090",
				MemoryInGb:  24,
				SecureCloud: true,
				LowestPrice: &gqlLowestPrice{
					UninterruptablePrice: &price,
					StockStatus:          &stock,
				},
			},
		}, nil
	}

	client := newCloudClientForTests(newCLIRunner("runpodctl"))
	client.cloudType = cloud.RunpodCloudTypeSecure
	offers, err := client.SearchOffers(cloud.OfferConstraints{})
	if err != nil {
		t.Fatalf("SearchOffers: %v", err)
	}
	if len(offers) != 1 || offers[0].CostPerHour != 0.74 {
		t.Fatalf("offers = %+v, want secure-priced offer", offers)
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

func TestShowUserErrorDoesNotDuplicateUserPrefix(t *testing.T) {
	runner := &cliRunner{
		cliPath:  "runpodctl",
		lookPath: func(string) (string, error) { return "/usr/bin/runpodctl", nil },
		runCombined: func(_ context.Context, _ string, args ...string) ([]byte, error) {
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
		runOutput: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if strings.Join(args, " ") != "user" {
				t.Fatalf("unexpected output command %q", strings.Join(args, " "))
			}
			return nil, errors.New("user: signal: killed")
		},
	}

	_, err := newCloudClientForTests(runner).ShowUser()
	if err == nil {
		t.Fatal("ShowUser() err = nil, want error")
	}
	if strings.Contains(err.Error(), "user: user:") {
		t.Fatalf("ShowUser() err = %q, should not duplicate user prefix", err.Error())
	}
}

func TestPodFromMapCapturesMachineMetadata(t *testing.T) {
	pod := podFromMap(map[string]any{
		"id":           "pod-123",
		"status":       "RUNNING",
		"dataCenterId": "EU-SE-1",
		"machine": map[string]any{
			"id": "machine-9",
		},
	})
	inst := podToInstance(pod)
	if inst.MachineID != "machine-9" {
		t.Fatalf("MachineID = %q, want machine-9", inst.MachineID)
	}
	if inst.DataCenter != "EU-SE-1" {
		t.Fatalf("DataCenter = %q, want EU-SE-1", inst.DataCenter)
	}
}

func TestPodFromMapCapturesNestedDataCenter(t *testing.T) {
	pod := podFromMap(map[string]any{
		"id":     "pod-123",
		"status": "RUNNING",
		"machine": map[string]any{
			"id": "machine-9",
			"dataCenter": map[string]any{
				"id": "US-KS-2",
			},
		},
	})
	if pod.MachineID != "machine-9" {
		t.Fatalf("MachineID = %q, want machine-9", pod.MachineID)
	}
	if pod.DataCenter != "US-KS-2" {
		t.Fatalf("DataCenter = %q, want US-KS-2", pod.DataCenter)
	}
}

func TestShowInstanceBackfillsMachineMetadataFromDetail(t *testing.T) {
	prev := fetchPodDetailFunc
	t.Cleanup(func() { fetchPodDetailFunc = prev })
	fetchPodDetailFunc = func(_ context.Context, podID string) (*Pod, error) {
		if podID != "pod-123" {
			t.Fatalf("podID = %q, want pod-123", podID)
		}
		return &Pod{MachineID: "machine-9", DataCenter: "US-KS-2"}, nil
	}

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
			if strings.Join(args, " ") == "pod get pod-123" {
				return []byte(`{"id":"pod-123","desiredStatus":"RUNNING"}`), nil
			}
			t.Fatalf("unexpected output command %q", strings.Join(args, " "))
			return nil, nil
		},
	}

	inst, err := newCloudClientForTests(runner).ShowInstance("pod-123")
	if err != nil {
		t.Fatalf("ShowInstance: %v", err)
	}
	if inst.MachineID != "machine-9" {
		t.Fatalf("MachineID = %q, want machine-9", inst.MachineID)
	}
	if inst.DataCenter != "US-KS-2" {
		t.Fatalf("DataCenter = %q, want US-KS-2", inst.DataCenter)
	}
}

func TestWaitReadyReturnsNotFoundAfterPodDisappears(t *testing.T) {
	prev := fetchPodDetailFunc
	t.Cleanup(func() { fetchPodDetailFunc = prev })
	fetchPodDetailFunc = func(context.Context, string) (*Pod, error) {
		return nil, errors.New("detail unavailable")
	}

	calls := 0
	runner := newSequencedPodGetRunner(t, func() ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`{"id":"pod-123","desiredStatus":"STARTING"}`), nil
		}
		return nil, errors.New("pod not found")
	})

	_, err := newCloudClientForTests(runner).waitReady("pod-123", 100*time.Millisecond, time.Millisecond)
	if !errors.Is(err, cloud.ErrInstanceNotFound) {
		t.Fatalf("WaitReady error = %v, want ErrInstanceNotFound", err)
	}
}

func TestWaitReadyAllowsInitialPodNotFound(t *testing.T) {
	prev := fetchPodDetailFunc
	t.Cleanup(func() { fetchPodDetailFunc = prev })
	fetchPodDetailFunc = func(context.Context, string) (*Pod, error) {
		return nil, errors.New("detail unavailable")
	}

	calls := 0
	runner := newSequencedPodGetRunner(t, func() ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("pod not found")
		}
		return []byte(`{"id":"pod-123","desiredStatus":"RUNNING"}`), nil
	})

	inst, err := newCloudClientForTests(runner).waitReady("pod-123", 100*time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if inst.ProviderID != "pod-123" || inst.Status != cloud.ProviderStatusRunning {
		t.Fatalf("instance = %+v, want running pod-123", inst)
	}
}

func newSequencedPodGetRunner(t *testing.T, podGet func() ([]byte, error)) *cliRunner {
	t.Helper()
	const path = "/opt/homebrew/bin/runpodctl"
	return &cliRunner{
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
			if strings.Join(args, " ") == "pod get pod-123" {
				return podGet()
			}
			t.Fatalf("unexpected output command %q", strings.Join(args, " "))
			return nil, nil
		},
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

func TestProbeDriverVersion(t *testing.T) {
	runner := newStubRunner(t,
		map[string]stubCLIResponse{
			"version":         {out: []byte("runpodctl 2.1.6")},
			"get --help":      {out: []byte("Available Commands:\n  cloud\n  pod\n")},
			"gpu --help":      {out: []byte("Available Commands:\n  list\n")},
			"pod --help":      {out: []byte("Available Commands:\n  create\n  delete\n  get\n  list\n")},
			"template --help": {out: []byte("Available Commands:\n  create\n  get\n  list\n")},
		},
		map[string]stubCLIResponse{
			"ssh info pod-123": {out: []byte(`{"command":"ssh -i /tmp/RunPod-Key-Go -p 22000 root@1.2.3.4"}`)},
		},
	)
	client := newCloudClientForTests(runner)

	var gotCommand string
	client.runLocalCommand = func(_ context.Context, command string) ([]byte, error) {
		gotCommand = command
		return []byte("550.127.05\n"), nil
	}

	got, err := client.ProbeDriverVersion(context.Background(), "pod-123")
	if err != nil {
		t.Fatalf("ProbeDriverVersion: %v", err)
	}
	if got != "550.127.05\n" {
		t.Fatalf("driver output = %q, want 550.127.05", got)
	}
	for _, want := range []string{
		"-o BatchMode=yes",
		"-o StrictHostKeyChecking=no",
		"nvidia-smi --query-gpu=driver_version --format=csv,noheader",
	} {
		if !strings.Contains(gotCommand, want) {
			t.Fatalf("probe command %q missing %q", gotCommand, want)
		}
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
