package edgeview

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/osteele/weft/internal/edge"
)

// Producer builds one section's bytes. A producer that fails does not stop
// the other sections: the failure is recorded in the manifest's errors map
// and the section is left out of the stamps.
type Producer func(ctx context.Context) ([]byte, error)

// Publisher writes the hub view. It is a hub component and runs in no other
// role.
type Publisher struct {
	transport    edge.Transport
	hubHost      string
	sourceDigest string
	now          func() time.Time
}

// NewPublisher returns a Publisher writing to transport. sourceDigest
// identifies the deployment that produced the view (the hub's build version).
func NewPublisher(transport edge.Transport, hubHost, sourceDigest string) *Publisher {
	return &Publisher{
		transport:    transport,
		hubHost:      hubHost,
		sourceDigest: sourceDigest,
		now:          time.Now,
	}
}

// Publish runs every producer, writes each produced section, and writes the
// manifest last, so a reader never sees a manifest naming a section that is
// not yet present. Producer failures are recorded in the returned manifest's
// Errors map and do not fail the publish; a failure to write to the store
// does.
func (p *Publisher) Publish(ctx context.Context, sections map[string]Producer) (*Manifest, error) {
	manifest := &Manifest{
		SchemaVersion: SchemaVersion,
		HubHost:       p.hubHost,
		SourceDigest:  p.sourceDigest,
		Sections:      map[string]SectionStamp{},
	}
	for _, name := range sortedKeys(sections) {
		body, err := sections[name](ctx)
		if err != nil {
			if manifest.Errors == nil {
				manifest.Errors = map[string]string{}
			}
			manifest.Errors[name] = err.Error()
			continue
		}
		if err := p.transport.Put(ctx, SectionKey(name), body); err != nil {
			return nil, fmt.Errorf("publish section %s: %w", name, err)
		}
		manifest.Sections[name] = SectionStamp{PublishedAt: p.now(), Bytes: len(body)}
	}
	manifest.PublishedAt = p.now()
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	if err := p.transport.Put(ctx, ManifestKey, data); err != nil {
		return nil, fmt.Errorf("publish manifest: %w", err)
	}
	return manifest, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
