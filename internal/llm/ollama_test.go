package llm

import (
	"testing"
)

func TestClientAvailability(t *testing.T) {
	client := NewDefaultClient()
	if !client.IsAvailable() {
		t.Skip("Ollama not available")
	}
	t.Log("Ollama is available")
}

func TestGenerateDescription(t *testing.T) {
	client := NewDefaultClient()
	if !client.IsAvailable() {
		t.Skip("Ollama not available")
	}

	desc, hash, err := client.GenerateDescription("uv run compression-lab report --data weights.safetensors --output-dir output/")
	if err != nil {
		t.Fatalf("GenerateDescription failed: %v", err)
	}

	t.Logf("Description: %s", desc)
	t.Logf("Hash: %s", hash)

	if desc == "" {
		t.Error("Description should not be empty")
	}
	if hash == "" {
		t.Error("Hash should not be empty")
	}
}
