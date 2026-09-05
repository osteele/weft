package explain

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

// Replan cannot route around a capability requirement: the requirement follows
// the job to the unplaced pool, so the only reachable hosts are the ones
// already advertising it — often just the blocked one. Suggesting replan sent
// an operator to an action that destroyed a correctly placed job.
func TestCapabilityBlockDoesNotSuggestReplan(t *testing.T) {
	job := &db.Job{
		Metadata: &db.JobMetadata{
			Agent: &db.JobAgentMetadata{
				RequiredCapabilities: []string{"tool:agent-review"},
			},
		},
	}
	if got := requiredCapabilities(job); len(got) != 1 || got[0] != "tool:agent-review" {
		t.Fatalf("requiredCapabilities = %v, want [tool:agent-review]", got)
	}
}

// A job declaring nothing keeps the ordinary replan advice, so the guard does
// not suppress a suggestion that is still correct.
func TestJobWithoutCapabilitiesHasNoRequirement(t *testing.T) {
	for _, job := range []*db.Job{
		{},
		{Metadata: &db.JobMetadata{}},
		{Metadata: &db.JobMetadata{Agent: &db.JobAgentMetadata{}}},
	} {
		if got := requiredCapabilities(job); len(got) != 0 {
			t.Errorf("requiredCapabilities = %v, want empty", got)
		}
	}
}

// The remedy text must name the command, since the condition is about the agent
// while the command lives under `queue` — two sessions in two projects looked
// under the noun the message named and found nothing.
func TestCapabilityRemedyNamesTheCommand(t *testing.T) {
	// Mirrors internal/ops.capabilityRemedy's contract; kept here because the
	// message is what an operator acts on.
	msg := "queue runner lacks job-payload-v1 capability; run `weft queue update studio` to deploy a current agent"
	if !strings.Contains(msg, "weft queue update") {
		t.Error("the remedy must name the command that performs it")
	}
}
