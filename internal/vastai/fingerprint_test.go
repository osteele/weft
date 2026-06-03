package vastai

import (
	"errors"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

// TestClassifyDriverVersBadField is the regression for the just-shipped UX
// bug: a Vast.ai 400 on "driver_version>=535" returns an error message naming
// the offending field. The fingerprint must collapse to a stable
// "vastai/search-offers/400/bad-field:driver_vers" so 18 jobs with different
// filter prefixes coalesce into one TUI incident instead of fragmenting.
func TestClassifyDriverVersBadField(t *testing.T) {
	pe := classify("search-offers", cloud.ErrProviderRejected, 400,
		" ask_contract_offers.driver_vers gte None: query values can't be None")
	if pe == nil {
		t.Fatal("classify returned nil")
	}
	if !errors.Is(pe, cloud.ErrProviderRejected) {
		t.Fatalf("errors.Is(pe, ErrProviderRejected) = false")
	}
	want := "vastai/search-offers/400/bad-field:driver_vers"
	if pe.Fingerprint != want {
		t.Fatalf("Fingerprint = %q, want %q", pe.Fingerprint, want)
	}
	if pe.StatusCode != 400 {
		t.Fatalf("StatusCode = %d, want 400", pe.StatusCode)
	}
	if pe.Op != "search-offers" {
		t.Fatalf("Op = %q, want search-offers", pe.Op)
	}
}

func TestClassifyUnknownSearchKey(t *testing.T) {
	pe := classify("search-offers", cloud.ErrProviderRejected, 400,
		"bogus_field is not a valid search key")
	if pe.Fingerprint != "vastai/search-offers/400/bad-field:bogus_field" {
		t.Fatalf("Fingerprint = %q", pe.Fingerprint)
	}
}

func TestClassifyAccountCreditExhausted(t *testing.T) {
	// Substring-derived: the sentinel passed in is ErrProviderRejected (what
	// the boundary saw) but the message text triggers the credit class.
	pe := classify("create-instance", cloud.ErrProviderRejected, 0,
		"failed with error 400: Your account lacks credit; see the billing page.")
	if pe.Fingerprint != "vastai/create-instance/account-credit-exhausted" {
		t.Fatalf("Fingerprint = %q", pe.Fingerprint)
	}
}

func TestClassifyAccountCreditExhaustedBySentinel(t *testing.T) {
	pe := classify("create-instance", cloud.ErrAccountCreditExhausted, 0, "any message")
	if pe.Fingerprint != "vastai/create-instance/account-credit-exhausted" {
		t.Fatalf("Fingerprint = %q", pe.Fingerprint)
	}
	if !errors.Is(pe, cloud.ErrAccountCreditExhausted) {
		t.Fatalf("errors.Is(ErrAccountCreditExhausted) = false")
	}
}

func TestClassifySentinelCases(t *testing.T) {
	cases := []struct {
		name     string
		op       string
		sentinel error
		want     string
	}{
		{"timeout", "search-offers", cloud.ErrProviderCommandTimeout, "vastai/search-offers/cli/timeout"},
		{"instance-not-found", "destroy-instance", cloud.ErrInstanceNotFound, "vastai/destroy-instance/instance-not-found"},
		{"offer-unavailable", "create-instance", cloud.ErrOfferUnavailable, "vastai/create-instance/offer-unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := classify(tc.op, tc.sentinel, 0, "")
			if pe.Fingerprint != tc.want {
				t.Fatalf("Fingerprint = %q, want %q", pe.Fingerprint, tc.want)
			}
			if !errors.Is(pe, tc.sentinel) {
				t.Fatalf("errors.Is(%v) = false", tc.sentinel)
			}
		})
	}
}

func TestClassifyUnknownFallback(t *testing.T) {
	pe := classify("search-offers", cloud.ErrProviderRejected, 400, "some unstructured complaint we haven't seen")
	if pe.Fingerprint != "vastai/search-offers/400/unknown" {
		t.Fatalf("Fingerprint = %q", pe.Fingerprint)
	}
}

func TestClassifyAuth(t *testing.T) {
	pe := classify("show-user", cloud.ErrProviderRejected, 401, "authentication required")
	if pe.Fingerprint != "vastai/show-user/401/auth" {
		t.Fatalf("Fingerprint = %q", pe.Fingerprint)
	}
}

func TestClassifyServer5xx(t *testing.T) {
	pe := classify("search-offers", cloud.ErrProviderRejected, 503, "service unavailable")
	if pe.Fingerprint != "vastai/search-offers/503/server" {
		t.Fatalf("Fingerprint = %q", pe.Fingerprint)
	}
}

func TestClassifyUnclassifiedWithoutStatus(t *testing.T) {
	pe := classify("search-offers", cloud.ErrProviderRejected, 0, "")
	if pe.Fingerprint != "vastai/search-offers/unclassified" {
		t.Fatalf("Fingerprint = %q", pe.Fingerprint)
	}
}

func TestOpNameFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"search offers raw", []string{"search", "offers", "--raw"}, "search-offers"},
		{"destroy instance with id", []string{"destroy", "instance", "12345", "-y", "--raw"}, "destroy-instance"},
		{"show user", []string{"show", "user", "--raw"}, "show-user"},
		{"create instance with id", []string{"create", "instance", "987654"}, "create-instance"},
		{"empty", []string{}, ""},
		{"flags only", []string{"--global", "-y"}, ""},
		// Regression: vastai search offers takes the whole filter as a single
		// positional arg, so opNameFromArgs must not let it through.
		{"search offers with filter", []string{"search", "offers", "--raw", "gpu_ram>=10 num_gpus=1 verified=true"}, "search-offers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := opNameFromArgs(tc.args); got != tc.want {
				t.Fatalf("opNameFromArgs(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestUpgradeSentinelPreservesOpAndStatus(t *testing.T) {
	// boundary saw a ProviderRejected with a status; callsite reclassifies as
	// AccountCreditExhausted (text-based). Op + status + fingerprint should
	// stay at the upstream truth modulo the new class.
	original := classify("create-instance", cloud.ErrProviderRejected, 400,
		"failed with error 400: Your account lacks credit; see the billing page.")
	upgraded := upgradeSentinel("create-instance", cloud.ErrAccountCreditExhausted, original)
	if upgraded.Op != "create-instance" {
		t.Fatalf("Op = %q, want create-instance", upgraded.Op)
	}
	if upgraded.StatusCode != 400 {
		t.Fatalf("StatusCode = %d, want 400 preserved", upgraded.StatusCode)
	}
	if upgraded.Fingerprint != "vastai/create-instance/account-credit-exhausted" {
		t.Fatalf("Fingerprint = %q", upgraded.Fingerprint)
	}
	if !errors.Is(upgraded, cloud.ErrAccountCreditExhausted) {
		t.Fatalf("errors.Is(ErrAccountCreditExhausted) = false")
	}
}

func TestUpgradeSentinelOnPlainError(t *testing.T) {
	plain := errors.New("plain text error")
	upgraded := upgradeSentinel("create-instance", cloud.ErrProviderRejected, plain)
	if upgraded.Op != "create-instance" {
		t.Fatalf("Op = %q", upgraded.Op)
	}
	if upgraded.Message != "plain text error" {
		t.Fatalf("Message = %q", upgraded.Message)
	}
}
