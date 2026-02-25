package llm

import (
	"context"
	"os"
	"strings"
)

// Generator defines the interface for LLM backends.
type Generator interface {
	IsAvailable() bool
	GenerationHash(prompt string) string
	Generate(ctx context.Context, prompt string) (string, error)
}

// NewDefaultClient selects the best available LLM backend.
// Prefers OpenRouter if OPENROUTER_API_KEY or OPENROUTER_KEY is set.
func NewDefaultClient() Generator {
	if key := firstNonEmpty(os.Getenv("OPENROUTER_API_KEY"), os.Getenv("OPENROUTER_KEY")); key != "" {
		cfg := OpenRouterConfig{
			APIKey: key,
			Model:  firstNonEmpty(os.Getenv("OPENROUTER_MODEL"), DefaultOpenRouterModel),
		}
		if baseURL := strings.TrimSpace(os.Getenv("OPENROUTER_BASE_URL")); baseURL != "" {
			cfg.BaseURL = baseURL
		}
		if ref := strings.TrimSpace(os.Getenv("OPENROUTER_APP_URL")); ref != "" {
			cfg.AppURL = ref
		}
		if title := strings.TrimSpace(os.Getenv("OPENROUTER_APP_TITLE")); title != "" {
			cfg.AppTitle = title
		}
		if cfg.AppTitle == "" {
			cfg.AppTitle = "weft"
		}
		return NewOpenRouterClient(cfg)
	}

	return NewOllamaClient(DefaultOllamaConfig())
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
