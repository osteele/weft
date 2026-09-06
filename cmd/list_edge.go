package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/edgeview"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

func init() {
	edgeViewStaticProducers[edgeview.SectionJobsIndex] = produceJobsIndexSection
}

// produceJobsIndexSection builds the jobs index section: the default listing
// a bare `weft list` prints, as a fully populated model. The publisher runs
// in the daemon, which never parses list flags, so the package-level flag
// variables hold their defaults; only the bare-list ordering is set
// explicitly.
func produceJobsIndexSection(ctx context.Context, deps edgeViewDeps) ([]byte, error) {
	listNewestFirst = true
	defer func() { listNewestFirst = false }()
	model, err := buildJobListModel(deps.DB, nil)
	if err != nil {
		return nil, err
	}
	return marshalViewJSON(model)
}

// runListEdge renders the jobs index on an edge. The published model is the
// default listing; narrowing filters (status, host, tags, project, a smaller
// --limit) are pure functions of the rows and run locally. Anything that
// would reach beyond the published window is refused with the bound named —
// never rendered as an empty or partial listing presented as complete.
func runListEdge(cmd *cobra.Command, args []string, em *edgeMirrorRuntime) error {
	if err := checkListEdgeFlags(args); err != nil {
		return err
	}
	body, prov, err := em.fetch(cmd, edgeview.SectionJobsIndex)
	if err != nil {
		return err
	}
	var model jobListView
	if err := json.Unmarshal(body, &model); err != nil {
		return fmt.Errorf("decode hub view section %s: %w", edgeview.SectionJobsIndex, err)
	}

	jobs, selection, err := filterJobListEdge(model.Jobs, model.Selection)
	if err != nil {
		return err
	}
	printListSelectionNotes(cmd.ErrOrStderr(), selection)

	switch listFormat {
	case "json":
		cols, err := terminal.ResolveColumns(listColumns, terminal.DefaultJSONColumnKeys)
		if err != nil {
			return err
		}
		return writeJobListJSON(cmd.OutOrStdout(), jobs, cols, selection, &prov)
	case "tsv", "tab":
		cols, err := terminal.ResolveColumns(listColumns, terminal.DefaultTSVColumnKeys)
		if err != nil {
			return err
		}
		if err := terminal.PrintJobsTSV(cmd.OutOrStdout(), jobs, cols); err != nil {
			return err
		}
	case "table", "":
		if _, err := fmt.Fprint(cmd.OutOrStdout(),
			terminal.RenderJobListPlainWithOptions(jobs, terminal.ListOutputWidth(), listColumns, listNoTruncate)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown format %q (use table, json, or tsv)", listFormat)
	}
	printEdgeProvenance(cmd.OutOrStdout(), prov)
	return nil
}

// checkListEdgeFlags refuses the flag combinations the published view cannot
// answer honestly. Each message names the bound it would cross.
func checkListEdgeFlags(args []string) error {
	switch {
	case listCleanup > 0:
		return fmt.Errorf("--cleanup deletes ledger rows; it is disabled on an edge")
	case listShowRaw != "" || listShow > 0:
		return fmt.Errorf("--show reads full job detail; use `weft info <id>` on an edge")
	case listTUI || listWatch:
		return fmt.Errorf("interactive displays read the live ledger; run them on the hub, not on an edge")
	case listGroupBy != "":
		return fmt.Errorf("--group-by %s needs per-launch state the hub view does not carry; run it on the hub", listGroupBy)
	case len(args) > 0:
		return fmt.Errorf("the hub view carries the default listing, not per-ID selection; use `weft status <id>` or `weft info <id>` on an edge")
	case listAll:
		return fmt.Errorf("the hub view carries the last %d days of jobs; --all needs the full ledger — run it on the hub", defaultListMaxAgeDays)
	case listSince != "":
		return fmt.Errorf("the hub view carries the last %d days of jobs; --since reaches beyond it — run it on the hub", defaultListMaxAgeDays)
	case listSearch != "":
		return fmt.Errorf("search runs as a ledger query; the hub view carries only the default listing — run the search on the hub")
	case listLimit <= 0 || listLimit > defaultListLimit:
		return fmt.Errorf("the hub view carries the first %d jobs of the default window; --limit %d reaches beyond it — run it on the hub", defaultListLimit, listLimit)
	}
	return nil
}

// filterJobListEdge applies the narrowing filters to the published rows and
// derives the selection metadata for the narrowed set. Counts that need the
// un-windowed ledger (how many older jobs the window hid for THIS filter) are
// unknown on the edge, so a narrowed selection carries no hidden counts and
// reports complete=false rather than a number nobody computed.
func filterJobListEdge(jobs []*db.Job, published jobListJSONSelection) ([]*db.Job, jobListJSONSelection, error) {
	statusFilter, processedFilter, failedOnly, err := listFilters()
	if err != nil {
		return nil, published, err
	}
	wantRental, wantInventory, err := listPlacementFlags()
	if err != nil {
		return nil, published, err
	}

	narrowed := statusFilter != "" || processedFilter != "" || failedOnly ||
		listHost != "" || len(listTags) > 0 || len(listExcludeTags) > 0 ||
		listProject != "" || listMine || wantRental || wantInventory || listActive
	if !narrowed && listLimit == defaultListLimit {
		return jobs, published, nil
	}

	filtered := jobsWithEffectiveStatus(jobs, statusFilter)
	filtered = filterJobsByFailureState(filtered, failedOnly)
	filtered = db.FilterJobsByTags(filtered, listTags, processedFilter)
	filtered = filterJobsByHostFlag(filtered)
	filtered = db.FilterJobsByExcludedTags(filtered, listExcludeTags)
	filtered = db.FilterJobsByProject(filtered, listProject)
	filtered = filterJobsByMine(filtered)
	filtered = filterJobsByPlacementScope(filtered, wantRental, wantInventory)
	if listActive {
		filtered = filterActiveJobs(filtered)
	}

	constraints := make([]jobListJSONConstraint, 0, 2)
	for _, constraint := range published.Constraints {
		if constraint.Kind == jobListJSONConstraintMaxAgeDays {
			// The window still bounds what the view carries; how many rows it
			// hid for this filter is unknown, so the count is dropped rather
			// than borrowed from the unfiltered listing.
			constraints = append(constraints, jobListJSONConstraint{
				Kind:      constraint.Kind,
				Value:     constraint.Value,
				Requested: true,
			})
		}
	}
	if listLimit > 0 && len(filtered) > listLimit {
		constraints = append(constraints, newJobListJSONConstraint(
			jobListJSONConstraintLimit, listLimit, len(filtered)-listLimit, true))
		filtered = filtered[:listLimit]
	}
	return filtered, newJobListJSONSelection(published.Order, constraints), nil
}
