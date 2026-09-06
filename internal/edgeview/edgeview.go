// Package edgeview is the hub view: a projection of the hub's ledger published
// to an object store and read by edge hosts. The hub's daemon publishes each
// section with the same serializers its own commands render from; an edge reads
// sections and renders them with those same renderers, so parity is a property
// of the pipeline rather than a goal.
//
// Freshness is the reachability signal: an edge cannot ask the hub whether it
// is alive, so "reachable" means the store answers, the manifest exists, and
// the section a command needs was published within the edge's staleness bound.
// A missing or stale section is never returned as empty bytes — unknown is
// never drawn as an empty result.
package edgeview

import (
	"fmt"
	"time"
)

// SchemaVersion is the view schema this build reads and writes. A reader
// refuses any other version rather than parsing leniently.
const SchemaVersion = 1

// Key layout. Sections are content the hub replaced wholesale on each publish;
// the manifest names every section with its stamp and is written last, so a
// reader never sees a manifest naming a section that is not yet present.
const (
	Prefix      = "view/v1/"
	ManifestKey = Prefix + "manifest.json"
)

// Section keys, relative to Prefix. jobs/<wj> sections are per job and are
// built with JobDetailSection / JobLogTailSection.
const (
	SectionJobsIndex = "jobs/index.json"
	SectionHosts     = "hosts.json"
	SectionAutopilot = "autopilot.json"
	SectionIncidents = "incidents.json"
)

// JobDetailSection is the section key for one job's detail document.
func JobDetailSection(jobID string) string { return "jobs/" + jobID + ".json" }

// JobLogTailSection is the section key for one job's bounded log tail.
func JobLogTailSection(jobID string) string { return "jobs/" + jobID + "/log.tail" }

// LogTailBytes bounds the log tail the hub copies into the view.
const LogTailBytes = 256 * 1024

// SectionKey maps a section name to its object key.
func SectionKey(name string) string { return Prefix + name }

// SectionStamp is the manifest's record of one published section.
type SectionStamp struct {
	PublishedAt time.Time `json:"published_at"`
	Bytes       int       `json:"bytes"`
}

// Manifest describes one publication of the view. Sections carries a stamp
// per section the hub produced; Errors carries one entry per section the hub
// tried and failed to produce, so a section that failed independently is
// distinguishable on the edge from one the hub never attempted.
type Manifest struct {
	SchemaVersion int                     `json:"schema_version"`
	HubHost       string                  `json:"hub_host"`
	PublishedAt   time.Time               `json:"published_at"`
	SourceDigest  string                  `json:"source_digest,omitempty"`
	Sections      map[string]SectionStamp `json:"sections"`
	Errors        map[string]string       `json:"errors,omitempty"`
}

// Provenance travels with every rendered section: where the bytes came from
// and how old they are. Every mirrored render ends with these fields (a text
// line or a "source" key in JSON) so a displayed fact always carries its
// source.
type Provenance struct {
	HubHost     string        `json:"hub_host"`
	PublishedAt time.Time     `json:"published_at"`
	Age         time.Duration `json:"-"`
	AgeSeconds  float64       `json:"age_seconds"`
	Transport   string        `json:"transport"`
	// Stale is true only when the reader's AllowStale downgraded an
	// over-bound section to a render.
	Stale bool `json:"stale,omitempty"`
}

// Line renders the provenance line that ends every mirrored text output.
func (p Provenance) Line() string {
	return fmt.Sprintf("source: hub %s via %s, published %s ago",
		p.HubHost, p.Transport, FormatAge(p.Age))
}

// FormatAge renders an age the way the freshness messages and the provenance
// line do: whole seconds for fresh views, coarser units as the view ages.
func FormatAge(d time.Duration) string {
	switch {
	case d < 0:
		d = 0
		fallthrough
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
