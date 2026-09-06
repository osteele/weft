package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/edgeview"
	"github.com/spf13/cobra"
)

// The hub-view sections a mirror command can read without a job-id argument.
// Job-scoped sections are built from the command's argument at fetch time.
const (
	edgeMirrorJobDetail  = "jobs/<wj>.json"
	edgeMirrorJobLogTail = "jobs/<wj>/log.tail"
)

// edgeMirrorServed is the set of mirror-classified commands this build serves
// from the hub view, mapped to the section each reads. A mirror command not
// listed here still blocks with the phase 1 cause — it must never fall
// through to the ledger. edgeview_publish_test.go pins the other direction:
// every static section named here has a hub-side producer.
var edgeMirrorServed = map[string]string{
	"list":             edgeview.SectionJobsIndex,
	"list jobs":        edgeview.SectionJobsIndex,
	"job list":         edgeview.SectionJobsIndex,
	"host list":        edgeview.SectionHosts,
	"list hosts":       edgeview.SectionHosts,
	"autopilot status": edgeview.SectionAutopilot,
	"incidents":        edgeview.SectionIncidents,
	"status":           edgeMirrorJobDetail,
	"job status":       edgeMirrorJobDetail,
	"info":             edgeMirrorJobDetail,
	"show":             edgeMirrorJobDetail,
	"job info":         edgeMirrorJobDetail,
	"log":              edgeMirrorJobLogTail,
	"job log":          edgeMirrorJobLogTail,
}

// edgeMirrorRuntime is what the gate hands a served mirror command on an
// edge: the view reader, the freshness bound, and nothing else. It is set by
// applyEdgeGate before RunE runs and is nil in every other role and for every
// unserved command, so a command can branch on it as its first act.
type edgeMirrorRuntime struct {
	reader     *edgeview.Reader
	staleAfter time.Duration
	// publishInterval is the hub's publication cadence from configuration;
	// --follow re-reads its section on this cadence.
	publishInterval time.Duration
	// rawArgs are the full invocation arguments, flags included. RunE receives
	// positionals only, but the blocked point's JSON/text choice is made from
	// the flags the caller passed, so the gate preserves them here.
	rawArgs []string
}

var (
	// activeEdgeMirror is non-nil only while a served mirror command runs in
	// the edge role.
	activeEdgeMirror *edgeMirrorRuntime
	// edgeAllowStale is the global --allow-stale flag. Off the edge role it is
	// accepted and ignored.
	edgeAllowStale bool
	// edgeViewTransportOverride replaces the [edge.view] store with a
	// filesystem directory (the hidden --edge-view-transport flag). It exists
	// for tests and offline development; production reads R2.
	edgeViewTransportOverride string
)

// newEdgeMirrorRuntime builds the view reader for an edge invocation. A
// failure here is a blocked outcome, not a crash: the edge's view store being
// unconfigured is exactly the "half-configured deployment" the blocked point
// exists to describe.
func newEdgeMirrorRuntime(cfg *config.Config, allowStale bool, rawArgs []string) (*edgeMirrorRuntime, error) {
	transport, err := edgeViewTransport(cfg, edgeViewTransportOverride)
	if err != nil {
		return nil, err
	}
	reader := edgeview.NewReader(transport)
	reader.AllowStale = allowStale
	return &edgeMirrorRuntime{
		reader:          reader,
		staleAfter:      cfg.Edge.View.StaleAfter(),
		publishInterval: cfg.Edge.View.PublishInterval(),
		rawArgs:         rawArgs,
	}, nil
}

// fetch returns one section's bytes and provenance. On any blocked outcome it
// emits the blocked point itself (JSON or text, following the invocation) and
// returns the gate error for RunE to propagate: the caller's duty on error is
// to return it unchanged.
func (m *edgeMirrorRuntime) fetch(cmd *cobra.Command, section string) ([]byte, edgeview.Provenance, error) {
	body, prov, err := m.reader.Section(cmd.Context(), section, m.staleAfter)
	if err == nil {
		if prov.Stale {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: hub view for %s is %s old (bound %s)\n",
				section, edgeview.FormatAge(prov.Age), edgeview.FormatAge(m.staleAfter))
		}
		return body, prov, nil
	}
	gateErr := &EdgeGateError{
		Outcome:  "blocked",
		Detail:   edgeViewCause(err),
		ExitCode: edgeExitBlocked,
	}
	var stale *edgeview.StaleError
	if errors.As(err, &stale) {
		age := stale.Age.Seconds()
		gateErr.ViewAgeS = &age
	}
	return nil, edgeview.Provenance{}, edgeBlock(cmd, m.rawArgs, gateErr)
}

// edgeViewCause renders a reader outcome as the blocked-point cause text.
func edgeViewCause(err error) string {
	switch {
	case errors.Is(err, edgeview.ErrNoManifest):
		return "no hub view has been published"
	default:
		return err.Error()
	}
}

// printEdgeProvenance ends a mirrored text render with its provenance line.
func printEdgeProvenance(w io.Writer, prov edgeview.Provenance) {
	fmt.Fprintln(w, prov.Line())
}
