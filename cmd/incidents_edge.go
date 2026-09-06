package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/osteele/weft/internal/edgeview"
	"github.com/spf13/cobra"
)

func init() {
	edgeViewStaticProducers[edgeview.SectionIncidents] = produceIncidentsSection
}

// produceIncidentsSection builds the incidents section: the incidents model
// encoded exactly as `weft incidents --json` encodes it, without provenance.
func produceIncidentsSection(ctx context.Context, deps edgeViewDeps) ([]byte, error) {
	incs, err := collectIncidents(deps.DB)
	if err != nil {
		return nil, err
	}
	return marshalViewJSON(incidentsView{Incidents: incs})
}

// runIncidentsEdge renders the incidents section on an edge: same render step
// as the hub, fed the hub's published bytes, with provenance attached.
func runIncidentsEdge(cmd *cobra.Command, args []string, em *edgeMirrorRuntime) error {
	body, prov, err := em.fetch(cmd, edgeview.SectionIncidents)
	if err != nil {
		return err
	}
	var view incidentsView
	if err := json.Unmarshal(body, &view); err != nil {
		return fmt.Errorf("decode hub view section %s: %w", edgeview.SectionIncidents, err)
	}
	if incidentsJSON {
		view.Source = &prov
		return writeIncidentsJSON(cmd.OutOrStdout(), view)
	}
	if err := renderIncidents(cmd.OutOrStdout(), view, false); err != nil {
		return err
	}
	printEdgeProvenance(cmd.OutOrStdout(), prov)
	return nil
}
