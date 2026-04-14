package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
)

func normalizeProviderFlag(raw string) (string, error) {
	provider := strings.ToLower(strings.TrimSpace(raw))
	if provider == "" {
		return "", nil
	}
	tag, err := db.ProviderTag(provider)
	if err != nil {
		return "", err
	}
	parsed, ok := db.RequestedProvider([]string{tag})
	if !ok {
		return "", fmt.Errorf("invalid provider %q", raw)
	}
	return parsed, nil
}

func withProviderTag(tags []string, provider string) ([]string, error) {
	provider, err := normalizeProviderFlag(provider)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(tags)+1)
	for _, tag := range tags {
		if _, ok := db.RequestedProvider([]string{tag}); ok {
			continue
		}
		out = append(out, tag)
	}

	if provider == "" {
		return out, nil
	}
	providerTag, err := db.ProviderTag(provider)
	if err != nil {
		return nil, err
	}
	return append(out, providerTag), nil
}
