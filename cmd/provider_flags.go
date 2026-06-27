package cmd

import (
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/cloud"
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

func normalizeRunpodCloudTypeFlag(raw string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	switch normalized {
	case "", "default", "auto", "none", "clear":
		return "", nil
	default:
		return cloud.NormalizeRunpodCloudType(raw)
	}
}

func applyRunpodCloudTypeProviderIntent(tags []string, host string, cloudType string) ([]string, error) {
	if strings.TrimSpace(cloudType) == "" {
		return tags, nil
	}
	if host != "" && !db.IsLaunchHost(host) {
		return nil, fmt.Errorf("--runpod-cloud-type=%s cannot be used with inventory host %q; omit --host to keep the job unplaced for RunPod rental placement", cloudType, host)
	}
	provider, ok := db.RequestedProvider(tags)
	if ok && provider != string(cloud.ProviderRunpod) {
		return nil, fmt.Errorf("--runpod-cloud-type=%s requires provider:runpod, but job requests provider:%s", cloudType, provider)
	}
	return withProviderTag(tags, string(cloud.ProviderRunpod))
}
