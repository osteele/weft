package runpod

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

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
