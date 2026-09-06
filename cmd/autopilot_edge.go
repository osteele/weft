package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/osteele/weft/internal/edgeview"
	"github.com/spf13/cobra"
)

func init() {
	edgeViewStaticProducers[edgeview.SectionAutopilot] = produceAutopilotSection
}

// produceAutopilotSection builds the autopilot status section: the hub's
// autopilot status model encoded exactly as --json encodes it.
func produceAutopilotSection(ctx context.Context, deps edgeViewDeps) ([]byte, error) {
	view, err := buildAutopilotStatusModel(deps.DB)
	if err != nil {
		return nil, err
	}
	return marshalViewJSON(view)
}

// runAutopilotStatusEdge renders the autopilot status section on an edge: the
// same model, decoded from the hub's published bytes, with provenance
// attached. --quiet keeps its exit-code contract on the view's model.
func runAutopilotStatusEdge(cmd *cobra.Command, args []string, em *edgeMirrorRuntime) error {
	body, prov, err := em.fetch(cmd, edgeview.SectionAutopilot)
	if err != nil {
		return err
	}
	var view autopilotStateView
	if err := json.Unmarshal(body, &view); err != nil {
		return fmt.Errorf("decode hub view section %s: %w", edgeview.SectionAutopilot, err)
	}
	if autopilotStatusQuiet {
		os.Exit(autopilotStateExitCode(view))
	}
	if autopilotStatusJSON {
		view.Source = &prov
		return writeAutopilotStatusJSON(cmd.OutOrStdout(), view)
	}
	fmt.Fprintln(cmd.OutOrStdout(), formatAutopilotStatusText(view))
	printEdgeProvenance(cmd.OutOrStdout(), prov)
	return nil
}
