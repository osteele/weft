package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/edgeview"
	"github.com/spf13/cobra"
)

func init() {
	edgeViewStaticProducers[edgeview.SectionHosts] = produceHostsSection
}

// produceHostsSection builds the hosts section: the full unfiltered host-list
// model (the --rentals filter is a render-time choice the edge applies
// locally), encoded as the hub's JSON encoder encodes it.
func produceHostsSection(ctx context.Context, deps edgeViewDeps) ([]byte, error) {
	cfg := deps.Cfg
	if cfg == nil {
		cfg = &config.Config{}
	}
	rows, err := loadHostListRows(time.Now(), deps.DB, cfg)
	if err != nil {
		return nil, err
	}
	return marshalViewJSON(hostListView{Hosts: rows})
}

// runHostListEdge renders the hosts section on an edge: the same render steps
// as the hub, fed the hub's published bytes, with provenance attached.
func runHostListEdge(cmd *cobra.Command, args []string, em *edgeMirrorRuntime, mode hostListRentalsMode) error {
	if hostListTUIFlag {
		return fmt.Errorf("the hosts TUI reads the live ledger directly; it runs on the hub, not on an edge")
	}
	body, prov, err := em.fetch(cmd, edgeview.SectionHosts)
	if err != nil {
		return err
	}
	var view hostListView
	if err := json.Unmarshal(body, &view); err != nil {
		return fmt.Errorf("decode hub view section %s: %w", edgeview.SectionHosts, err)
	}
	view.Hosts = filterHostListRows(view.Hosts, mode)
	if hostListJSONFlag {
		view.Source = &prov
		return writeHostListJSON(cmd.OutOrStdout(), view)
	}
	if err := writeHostListTable(cmd.OutOrStdout(), view.Hosts); err != nil {
		return err
	}
	printEdgeProvenance(cmd.OutOrStdout(), prov)
	return nil
}
