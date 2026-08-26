package secrets

import (
	"os"
	"regexp"
	"strings"
)

const redactedValue = "<redacted>"

var textRedactionPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{
		pattern:     regexp.MustCompile(`(?i)(\bAuthorization\s*[:=]\s*Bearer\s+)[^\s,;]+`),
		replacement: `${1}` + redactedValue,
	},
	{
		pattern:     regexp.MustCompile(`(?i)([?&](?:api[_-]?key|access[_-]?key|private[_-]?key|access[_-]?token|auth[_-]?token|token|secret|password|credential|signature|x-amz-(?:credential|signature|security-token))=)[^&\s"']+`),
		replacement: `${1}` + redactedValue,
	},
	{
		pattern:     regexp.MustCompile(`(?i)(--(?:api[_-]?key|access[_-]?key|private[_-]?key|access[_-]?token|auth[_-]?token|token|secret|password|credential)(?:=|\s+))[^\s,;]+`),
		replacement: `${1}` + redactedValue,
	},
	{
		pattern:     regexp.MustCompile(`(?i)("(?:api[_-]?key|access[_-]?key|private[_-]?key|access[_-]?token|auth[_-]?token|token|secret|password|credential)"\s*:\s*")[^"]*(")`),
		replacement: `${1}` + redactedValue + `${2}`,
	},
	{
		pattern:     regexp.MustCompile(`(?i)(\b[A-Za-z0-9_.-]*(?:api[_-]?key|access[_-]?key|private[_-]?key|access[_-]?token|auth[_-]?token|token|secret|password|credential)[A-Za-z0-9_.-]*\b\s*(?:=>|=|:)\s*["']?)[^"'\s,;&]+`),
		replacement: `${1}` + redactedValue,
	},
	{
		pattern:     regexp.MustCompile(`(?i)(\b(?:api[_ -]?key|access[_ -]?token|auth[_ -]?token|token|secret|password|credential)\s+)[A-Za-z0-9._~+/=-]{16,}`),
		replacement: `${1}` + redactedValue,
	},
	{
		pattern:     regexp.MustCompile(`(?i)(https?://[^:/\s]+:)[^@\s/]+@`),
		replacement: `${1}` + redactedValue + `@`,
	},
	{
		pattern:     regexp.MustCompile(`(?i)(https://hooks\.slack\.com/services/)[^\s"']+`),
		replacement: `${1}` + redactedValue,
	},
	{
		pattern:     regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|hf_[A-Za-z0-9]{20,}|gh[pousr]_[A-Za-z0-9]{20,})\b`),
		replacement: redactedValue,
	},
}

// RedactText removes credential values from text before it reaches logs,
// diagnostics, or other display surfaces.
func RedactText(text string) string {
	return redactTextWithEnv(text, os.Environ())
}

// RedactArgs removes credentials from command arguments while preserving the
// argument boundaries recorded by audit logs.
func RedactArgs(args []string) []string {
	redacted := make([]string, len(args))
	for i, arg := range args {
		redacted[i] = RedactText(arg)
	}
	for i, arg := range args {
		flagName := strings.TrimLeft(arg, "-")
		if name, _, ok := strings.Cut(flagName, "="); ok {
			flagName = name
		}
		flagName = strings.ReplaceAll(flagName, "-", "_")
		if LooksSecretKey(flagName) && !strings.Contains(arg, "=") && i+1 < len(args) {
			redacted[i+1] = redactedValue
		}
	}
	return redacted
}

func redactTextWithEnv(text string, env []string) string {
	redacted := text
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !LooksSecretKey(key) || len(value) < 8 {
			continue
		}
		redacted = strings.ReplaceAll(redacted, value, redactedValue)
	}
	for _, rule := range textRedactionPatterns {
		redacted = rule.pattern.ReplaceAllString(redacted, rule.replacement)
	}
	return redacted
}
