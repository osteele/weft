package cmd

import (
	"strings"
	"testing"
)

// An unknown status must be refused rather than silently matching nothing.
//
// Accepting any string makes every typo and every non-status word return an
// empty list indistinguishable from a genuine absence, which is how two
// sessions concluded that no jobs were blocked while a job was blocked.
func TestUnknownStatusFilterIsRefused(t *testing.T) {
	err := validateStatusFilter("notareal")
	if err == nil {
		t.Fatal("an unknown status was accepted; it will match nothing and read as an absence")
	}
	if !strings.Contains(err.Error(), "valid statuses") {
		t.Errorf("refusal should list the vocabulary: %v", err)
	}
}

// "blocked" is the case that cost real time: a condition rendered in the detail
// view, never a stored status, so the filter could never match it.
func TestBlockedIsRefusedWithItsOwnExplanation(t *testing.T) {
	err := validateStatusFilter("blocked")
	if err == nil {
		t.Fatal(`"blocked" was accepted as a status filter; it is never stored, so it matches nothing`)
	}
	if !strings.Contains(err.Error(), "not a stored job status") {
		t.Errorf("refusal should say why blocked is not filterable: %v", err)
	}
	if !strings.Contains(err.Error(), "weft status") {
		t.Errorf("refusal should point at the command that does show it: %v", err)
	}
}

// Every stored status must remain filterable, or the guard has broken the flag
// it was added to protect.
func TestEveryStoredStatusIsAccepted(t *testing.T) {
	for _, s := range []string{
		"draft", "pending_placement", "queued", "starting", "running",
		"completed", "failed", "dead", "killed", "canceled", "skipped", "paused", "unresolved",
	} {
		if err := validateStatusFilter(s); err != nil {
			t.Errorf("stored status %q was refused: %v", s, err)
		}
	}
}
