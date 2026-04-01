//go:build integration
// +build integration

package llm

import (
	"context"
	"fmt"
	"os"
	"testing"
)

func TestClientAvailability(t *testing.T) {
	if os.Getenv("LLM_TEST") == "" {
		t.Skip("set LLM_TEST=1 to run LLM integration tests")
	}

	client := NewDefaultClient()
	if !client.IsAvailable() {
		t.Skip("LLM backend not available")
	}
	t.Log("LLM backend is available")
}

func TestGenerateDescription(t *testing.T) {
	if os.Getenv("LLM_TEST") == "" {
		t.Skip("set LLM_TEST=1 to run LLM integration tests")
	}

	client := NewDefaultClient()
	if !client.IsAvailable() {
		t.Skip("LLM backend not available")
	}

	prompt := fmt.Sprintf(DefaultPromptTemplate, "uv run compression-lab report --data weights.safetensors --output-dir output/")
	desc, err := client.Generate(context.Background(), prompt)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	hash := client.GenerationHash(prompt)

	t.Logf("Description: %s", desc)
	t.Logf("Hash: %s", hash)

	if desc == "" {
		t.Error("Description should not be empty")
	}
	if hash == "" {
		t.Error("Hash should not be empty")
	}
}
