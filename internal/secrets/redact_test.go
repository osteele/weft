package secrets

import (
	"strings"
	"testing"
)

func TestRedactText(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "query parameter in provider traceback",
			input: "https://console.vast.ai/api/v0/instances?owner=me&api_key=" + secret,
		},
		{
			name:  "authorization header",
			input: "Authorization: Bearer " + secret,
		},
		{
			name:  "json field",
			input: `{"access_token":"` + secret + `"}`,
		},
		{
			name:  "command flag",
			input: "provider --api-key " + secret + " list",
		},
		{
			name:  "plain token diagnostic",
			input: "invalid token " + secret + ": missing discharge",
		},
		{
			name:  "url credentials",
			input: "https://user:" + secret + "@example.com/path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactTextWithEnv(tt.input, nil)
			if strings.Contains(got, secret) {
				t.Fatalf("RedactText left credential in %q", got)
			}
			if !strings.Contains(got, redactedValue) {
				t.Fatalf("RedactText = %q, want redaction marker", got)
			}
		})
	}
}

func TestRedactTextUsesSecretEnvironmentValues(t *testing.T) {
	const secret = "environment-only-secret-value"
	got := redactTextWithEnv("request failed for "+secret, []string{"Z_AI_API_KEY=" + secret, "PATH=/bin"})
	if strings.Contains(got, secret) || !strings.Contains(got, redactedValue) {
		t.Fatalf("redactTextWithEnv = %q", got)
	}
}

func TestRedactTextPreservesOrdinaryTokenLanguage(t *testing.T) {
	const input = "token budget exhausted; retry after login"
	if got := redactTextWithEnv(input, nil); got != input {
		t.Fatalf("redactTextWithEnv = %q, want %q", got, input)
	}
}

func TestRedactArgsHandlesSeparateFlagValue(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	got := RedactArgs([]string{"provider", "--api-key", secret, "list"})
	want := []string{"provider", "--api-key", redactedValue, "list"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RedactArgs = %q, want %q", got, want)
		}
	}
}
