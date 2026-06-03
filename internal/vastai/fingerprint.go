package vastai

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/osteele/weft/internal/cloud"
)

// fingerprint module
//
// Derives a stable error-class fingerprint for vastai provider failures so
// that systemic upstream issues (e.g. one bad filter clause causing every
// SearchOffers to 400) coalesce into a single incident in display surfaces
// instead of fragmenting into N look-alike buckets.
//
// Fingerprint format: "vastai/<op>/<class>[:<key>]" — see internal/cloud
// ProviderError docs for the cross-provider contract.

const providerName = "vastai"

// classify builds a *cloud.ProviderError for any vastai operation. op is the
// kebab-cased operation name ("search-offers", "create-instance",
// "destroy-instance", "show-user", "show-instance", "show-instances"); it
// becomes part of the fingerprint and the human-facing prefix. statusCode is
// 0 when not known. message is the upstream-provided text. The returned
// error wraps sentinel via Unwrap so existing errors.Is dispatch continues to
// work.
func classify(op string, sentinel error, statusCode int, message string) *cloud.ProviderError {
	classParts := classifyMessage(sentinel, statusCode, message)
	parts := append([]string{providerName, op}, classParts...)
	return cloud.WrapProviderError(sentinel, providerName, op, statusCode, message, parts...)
}

// upgradeSentinel reclassifies an existing error under a new sentinel while
// preserving the upstream context (op, status, message) when the error
// already carries a *cloud.ProviderError. The common case: CreateInstance
// post-processing decides an error originally tagged ErrProviderRejected is
// actually ErrAccountCreditExhausted — the structured fields and fingerprint
// keep their upstream truth; only the sentinel and class shift.
//
// When err carries no ProviderError, builds a fresh one tagged with op and
// the err.Error() text as message.
func upgradeSentinel(op string, sentinel error, err error) *cloud.ProviderError {
	if pe, ok := cloud.AsProviderError(err); ok {
		preserveOp := pe.Op
		if preserveOp == "" {
			preserveOp = op
		}
		message := pe.Message
		if strings.TrimSpace(message) == "" {
			message = err.Error()
		}
		return classify(preserveOp, sentinel, pe.StatusCode, message)
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	return classify(op, sentinel, 0, message)
}

// opNameFromArgs converts the argv-derived operation prefix into the
// kebab-cased identifier used in fingerprints. ["search", "offers"] →
// "search-offers"; ["create", "instance", "12345"] → "create-instance"
// (numeric IDs are dropped to keep cardinality bounded). Tokens that
// aren't operation-name-shaped (filter strings like `gpu_ram>=10 num_gpus=1`,
// paths, anything containing whitespace or operators) are also dropped — vast
// CLI takes the entire filter as a single positional argument and the
// runWithTimeout prefix capture sees it through, so without this filter the
// op name picks up the user's query.
func opNameFromArgs(args []string) string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		a = strings.TrimSpace(a)
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		if !isOpToken(a) {
			continue
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "-")
}

// isOpToken reports whether s looks like a vastai operation-name component
// ("search", "offers", "show", "user", "instance"). Anything containing
// digits, whitespace, or operator/punctuation characters is rejected — only
// ASCII letters and underscores survive.
func isOpToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_':
		default:
			return false
		}
	}
	return true
}

// classifyMessage maps (sentinel, statusCode, message) → fingerprint suffix
// parts ([]string, joined by BuildFingerprint with "/" + a trailing ":<key>"
// embedded in the last part when applicable).
//
// Order of precedence:
//  1. Sentinel-level classification (timeout, instance-not-found,
//     offer-unavailable) wins — these are categorical, not message-derived.
//  2. Credit exhaustion has a stable substring family (isAccountCreditError).
//  3. HTTP 4xx — extract the offending field from the message when the
//     pattern is one of a known set, else "<status>/unknown".
//  4. HTTP 5xx — bucket by status only.
//  5. Anything else — "unclassified".
func classifyMessage(sentinel error, statusCode int, message string) []string {
	switch sentinel {
	case cloud.ErrProviderCommandTimeout:
		return []string{"cli/timeout"}
	case cloud.ErrInstanceNotFound:
		return []string{"instance-not-found"}
	case cloud.ErrOfferUnavailable:
		return []string{"offer-unavailable"}
	case cloud.ErrAccountCreditExhausted:
		return []string{"account-credit-exhausted"}
	}
	if isAccountCreditError(message) {
		return []string{"account-credit-exhausted"}
	}
	switch {
	case statusCode >= 400 && statusCode < 500:
		if field := extractBadField(message); field != "" {
			return []string{fmt.Sprintf("%d/bad-field:%s", statusCode, field)}
		}
		if statusCode == 401 || statusCode == 403 {
			return []string{fmt.Sprintf("%d/auth", statusCode)}
		}
		return []string{fmt.Sprintf("%d/unknown", statusCode)}
	case statusCode >= 500 && statusCode < 600:
		return []string{fmt.Sprintf("%d/server", statusCode)}
	}
	return []string{"unclassified"}
}

// extractBadField recovers the offending search-key field name from one of
// the known vastai 400 error message shapes. Returns "" when no known
// pattern matches.
//
// Patterns observed in the wild:
//
//   - Search-filter mapping failure:
//     "ask_contract_offers.<field> gte None: query values can't be None"
//     (e.g. the driver_vers bug that motivated this whole module)
//
//   - Unknown search key:
//     "<field> is not a valid search key"
//     (e.g. typo in a filter clause, or vastai removed the field)
//
// Additional patterns can be added here as they surface. Each must produce
// a low-cardinality key — never embed numeric values or unbounded strings.
func extractBadField(message string) string {
	if message == "" {
		return ""
	}
	for _, re := range badFieldPatterns {
		if m := re.FindStringSubmatch(message); len(m) >= 2 {
			return sanitizeFieldName(m[1])
		}
	}
	return ""
}

var badFieldPatterns = []*regexp.Regexp{
	regexp.MustCompile(`ask_contract_offers\.([a-zA-Z_][a-zA-Z0-9_]*)\s+(?:gte|gt|lt|lte|eq|neq)\s+None`),
	regexp.MustCompile(`(?i)\b([a-zA-Z_][a-zA-Z0-9_]*)\s+is not a valid search key`),
}

// sanitizeFieldName guards against pathological message contents leaking
// into a fingerprint — only ASCII identifier characters survive.
func sanitizeFieldName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	const max = 48
	if len(s) > max {
		s = s[:max]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}
