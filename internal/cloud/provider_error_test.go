package cloud

import (
	"errors"
	"strings"
	"testing"
)

// TestProviderErrorIsPreservesSentinel verifies that wrapping a sentinel in a
// ProviderError does not break the errors.Is dispatch the retry classifiers
// (isRetryableCreateError, IsRetryableNewInstanceLaunchError) rely on.
func TestProviderErrorIsPreservesSentinel(t *testing.T) {
	pe := &ProviderError{
		Sentinel:   ErrProviderRejected,
		Provider:   "vastai",
		Op:         "search-offers",
		StatusCode: 400,
		Message:    "bogus_field is not a valid search key",
	}
	if !errors.Is(pe, ErrProviderRejected) {
		t.Fatalf("errors.Is(pe, ErrProviderRejected) = false, want true")
	}
	if errors.Is(pe, ErrOfferUnavailable) {
		t.Fatalf("errors.Is(pe, ErrOfferUnavailable) = true, want false")
	}
	if errors.Is(pe, ErrAccountCreditExhausted) {
		t.Fatalf("errors.Is(pe, ErrAccountCreditExhausted) = true, want false")
	}
}

// TestProviderErrorIsThroughWrap verifies sentinel dispatch survives one more
// wrap with %w by an outer caller (e.g. SearchOffers adding context).
func TestProviderErrorIsThroughWrap(t *testing.T) {
	pe := &ProviderError{Sentinel: ErrAccountCreditExhausted, Provider: "vastai", Op: "create-instance"}
	wrapped := errWrapForTest("create instance 42", pe)
	if !errors.Is(wrapped, ErrAccountCreditExhausted) {
		t.Fatalf("errors.Is(wrapped, ErrAccountCreditExhausted) = false, want true")
	}
	if !errors.Is(wrapped, pe) {
		t.Fatalf("errors.Is(wrapped, pe) = false, want true")
	}
}

// errWrapForTest is the minimal wrap pattern external callers use; kept local
// to avoid pulling fmt into the production file just for a test helper.
func errWrapForTest(msg string, err error) error {
	if err == nil {
		return nil
	}
	return &wrappedError{msg: msg, err: err}
}

type wrappedError struct {
	msg string
	err error
}

func (w *wrappedError) Error() string { return w.msg + ": " + w.err.Error() }
func (w *wrappedError) Unwrap() error { return w.err }

func TestProviderErrorAsExtracts(t *testing.T) {
	want := &ProviderError{
		Sentinel:    ErrProviderRejected,
		Provider:    "vastai",
		Op:          "search-offers",
		StatusCode:  400,
		Fingerprint: "vastai/search-offers/400/bad-field:driver_vers",
	}
	wrapped := errWrapForTest("planner", want)

	got, ok := AsProviderError(wrapped)
	if !ok {
		t.Fatalf("AsProviderError returned (_, false), want true")
	}
	if got != want {
		t.Fatalf("AsProviderError returned %p, want %p", got, want)
	}

	if fp := FingerprintOf(wrapped); fp != want.Fingerprint {
		t.Fatalf("FingerprintOf = %q, want %q", fp, want.Fingerprint)
	}
}

func TestProviderErrorAsNilAndNonProvider(t *testing.T) {
	if _, ok := AsProviderError(nil); ok {
		t.Fatalf("AsProviderError(nil) returned ok=true")
	}
	if _, ok := AsProviderError(errors.New("plain")); ok {
		t.Fatalf("AsProviderError(plain) returned ok=true")
	}
	if fp := FingerprintOf(nil); fp != "" {
		t.Fatalf("FingerprintOf(nil) = %q, want empty", fp)
	}
	if fp := FingerprintOf(errors.New("plain")); fp != "" {
		t.Fatalf("FingerprintOf(plain) = %q, want empty", fp)
	}
}

func TestProviderErrorErrorString(t *testing.T) {
	pe := &ProviderError{
		Sentinel: ErrProviderRejected,
		Provider: "vastai",
		Op:       "search-offers",
		Message:  "bogus_field is not a valid search key",
	}
	got := pe.Error()
	if !strings.Contains(got, "vastai search-offers") {
		t.Fatalf("Error() = %q, want provider/op tag", got)
	}
	if !strings.Contains(got, ErrProviderRejected.Error()) {
		t.Fatalf("Error() = %q, want sentinel text", got)
	}
	if !strings.Contains(got, "bogus_field is not a valid search key") {
		t.Fatalf("Error() = %q, want upstream message", got)
	}
	// Actionable message must lead so column-truncated displays preserve it.
	msgIdx := strings.Index(got, "bogus_field is not a valid search key")
	sentIdx := strings.Index(got, ErrProviderRejected.Error())
	if msgIdx < 0 || sentIdx < 0 || msgIdx > sentIdx {
		t.Fatalf("Error() = %q, want upstream message before sentinel", got)
	}
}

func TestBuildFingerprint(t *testing.T) {
	cases := []struct {
		name  string
		parts []string
		want  string
	}{
		{name: "empty", parts: nil, want: ""},
		{name: "single", parts: []string{"vastai"}, want: "vastai"},
		{name: "multi", parts: []string{"vastai", "search-offers", "400/bad-field:driver_vers"}, want: "vastai/search-offers/400/bad-field:driver_vers"},
		{name: "skips blanks", parts: []string{"vastai", "", "  ", "create-instance", "offer-unavailable"}, want: "vastai/create-instance/offer-unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildFingerprint(tc.parts...); got != tc.want {
				t.Fatalf("BuildFingerprint(%v) = %q, want %q", tc.parts, got, tc.want)
			}
		})
	}
}

func TestWrapProviderErrorRoundsTrip(t *testing.T) {
	pe := WrapProviderError(
		ErrProviderRejected,
		"vastai",
		"search-offers",
		400,
		"ask_contract_offers.driver_vers gte None: query values can't be None",
		"vastai", "search-offers", "400/bad-field:driver_vers",
	)
	if pe.Sentinel != ErrProviderRejected {
		t.Fatalf("Sentinel = %v, want ErrProviderRejected", pe.Sentinel)
	}
	if pe.Provider != "vastai" || pe.Op != "search-offers" || pe.StatusCode != 400 {
		t.Fatalf("WrapProviderError fields wrong: %+v", pe)
	}
	if pe.Fingerprint != "vastai/search-offers/400/bad-field:driver_vers" {
		t.Fatalf("Fingerprint = %q, want vastai/search-offers/400/bad-field:driver_vers", pe.Fingerprint)
	}
	if !errors.Is(pe, ErrProviderRejected) {
		t.Fatalf("errors.Is(pe, ErrProviderRejected) = false")
	}
}
