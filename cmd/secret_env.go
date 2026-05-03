package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/osteele/weft/internal/secrets"
	"github.com/spf13/cobra"
)

func formatEnvVarsForDisplay(envVars []string) string {
	return strings.Join(secrets.RedactEnvVars(envVars), ", ")
}

func addSecretEnvFlags(cmd *cobra.Command, hfToken *bool, hfTokenFrom *string, secretVars *[]string) {
	cmd.Flags().BoolVar(hfToken, "hf-token", false, "Attach HF_TOKEN from the stored 'hf' secret")
	cmd.Flags().StringVar(hfTokenFrom, "hf-token-from", "", "Attach HF_TOKEN from secret:<name> or env:<name>")
	cmd.Flags().StringSliceVar(secretVars, "secret", nil, "Attach a secret env var (KEY=secret:<name> or KEY=env:<name>), can be repeated")
}

func applySecretEnv(envVars []string, inputs []string, hfToken bool, hfTokenFrom string, secretVars []string) ([]string, error) {
	resolved := append([]string(nil), envVars...)
	for _, spec := range secretVars {
		key, ref, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(ref) == "" {
			return nil, fmt.Errorf("--secret expects KEY=secret:<name> or KEY=env:<name>")
		}
		value, err := secretEnvReference(strings.TrimSpace(key), strings.TrimSpace(ref))
		if err != nil {
			return nil, fmt.Errorf("--secret %s: %w", key, err)
		}
		resolved = setEnvVarByKey(resolved, strings.TrimSpace(key), value)
	}
	if hfTokenFrom != "" {
		value, err := hfTokenReference(hfTokenFrom)
		if err != nil {
			return nil, fmt.Errorf("--hf-token-from: %w", err)
		}
		resolved = setEnvVarByKey(resolved, "HF_TOKEN", value)
		return resolved, nil
	}
	if hfToken {
		value, err := hfTokenReference("secret:hf")
		if err != nil {
			return nil, fmt.Errorf("--hf-token: %w", err)
		}
		resolved = setEnvVarByKey(resolved, "HF_TOKEN", value)
		return resolved, nil
	}
	if needsHFToken(inputs) && !hasEnvVarKey(resolved, "HF_TOKEN") && secrets.Exists("hf") {
		resolved = append(resolved, "HF_TOKEN=secret:hf")
	}
	return resolved, nil
}

func hfTokenReference(ref string) (string, error) {
	switch {
	case ref == "":
		return "secret:hf", nil
	case strings.HasPrefix(ref, "secret:"):
		name := strings.TrimPrefix(ref, "secret:")
		if !secrets.Exists(name) {
			return "", fmt.Errorf("secret %q not found; run 'weft secret set %s' first", name, name)
		}
		return ref, nil
	case strings.HasPrefix(ref, "env:"):
		envName := strings.TrimPrefix(ref, "env:")
		value, ok := os.LookupEnv(envName)
		if !ok || value == "" {
			return "", fmt.Errorf("environment variable %s is empty or unset", envName)
		}
		if err := secrets.Set("hf", value); err != nil {
			return "", err
		}
		return "secret:hf", nil
	default:
		return "", fmt.Errorf("expected secret:<name> or env:<name>")
	}
}

func secretEnvReference(key, ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "secret:"):
		name := strings.TrimPrefix(ref, "secret:")
		if !secrets.Exists(name) {
			return "", fmt.Errorf("secret %q not found", name)
		}
		return ref, nil
	case strings.HasPrefix(ref, "env:"):
		envName := strings.TrimPrefix(ref, "env:")
		value, ok := os.LookupEnv(envName)
		if !ok || value == "" {
			return "", fmt.Errorf("environment variable %s is empty or unset", envName)
		}
		name := strings.ToLower(key)
		if err := secrets.Set(name, value); err != nil {
			return "", err
		}
		return "secret:" + name, nil
	default:
		return "", fmt.Errorf("expected secret:<name> or env:<name>")
	}
}

func needsHFToken(inputs []string) bool {
	for _, input := range inputs {
		if strings.HasPrefix(input, "hf:") || strings.HasPrefix(input, "hf-dataset:") {
			return true
		}
	}
	return false
}

func hasEnvVarKey(envVars []string, key string) bool {
	prefix := key + "="
	for _, ev := range envVars {
		if strings.HasPrefix(ev, prefix) {
			return true
		}
	}
	return false
}

func setEnvVarByKey(envVars []string, key, value string) []string {
	prefix := key + "="
	next := append([]string(nil), envVars...)
	for i, ev := range next {
		if strings.HasPrefix(ev, prefix) {
			next[i] = prefix + value
			return next
		}
	}
	return append(next, prefix+value)
}
