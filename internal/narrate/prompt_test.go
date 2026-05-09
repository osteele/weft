package narrate

import (
	"strings"
	"testing"
)

func TestSystemPromptExamplesAvoidSteadyStateClosers(t *testing.T) {
	for _, bad := range []string{
		"Everything else is steady",
		"Everything else is unchanged",
		"nothing else moved",
		"Quiet stretch",
		"Routine status (no transitions)",
	} {
		if strings.Contains(systemPrompt, bad) {
			t.Fatalf("systemPrompt contains stale narration example %q", bad)
		}
	}
}

func TestSystemPromptExamplesModelGraceTransitions(t *testing.T) {
	for _, want := range []string{
		"recovered from grace",
		"moved back into grace period",
		"Relevant progress",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("systemPrompt missing grace-transition example %q", want)
		}
	}
}
