package retrypolicy

import "testing"

func TestMaxAttemptsWithExtra(t *testing.T) {
	if got, want := MaxAttemptsWithExtra(0), MaxPlacementAttempts(); got != want {
		t.Fatalf("MaxAttemptsWithExtra(0) = %d, want %d", got, want)
	}
	if got, want := MaxAttemptsWithExtra(2), MaxPlacementAttempts()+2; got != want {
		t.Fatalf("MaxAttemptsWithExtra(2) = %d, want %d", got, want)
	}
	if got := MaxAttemptsWithExtra(-MaxPlacementAttempts()); got != 1 {
		t.Fatalf("MaxAttemptsWithExtra(negative) = %d, want floor 1", got)
	}
}
