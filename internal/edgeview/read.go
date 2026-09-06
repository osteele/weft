package edgeview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/edge"
)

// The reader's outcomes, as typed sentinel errors. A caller maps these to the
// blocked point; a Section call that returns one of them returns no bytes,
// because a missing or stale section is unknown, and unknown is never drawn
// as an empty result.
var (
	// ErrStoreUnreachable: the store could not be consulted at all.
	ErrStoreUnreachable = errors.New("view store unreachable")
	// ErrNoManifest: the store answered and no hub view has ever been published.
	ErrNoManifest = errors.New("no hub view has been published")
	// ErrManifestUnreadable: the manifest exists but does not parse.
	ErrManifestUnreadable = errors.New("hub view manifest is unreadable")
	// ErrUnknownSchema: the manifest's schema version is not one this build reads.
	ErrUnknownSchema = errors.New("unsupported hub view schema version")
	// ErrSectionAbsent: the manifest does not name the section. When the hub
	// tried and failed to produce it, the manifest's errors entry is included
	// in the message.
	ErrSectionAbsent = errors.New("section absent from hub view manifest")
	// ErrSectionStale: the section's stamp is older than the freshness bound.
	ErrSectionStale = errors.New("hub view section older than freshness bound")
	// ErrSectionMissing: the manifest names the section but the object is not
	// in the store — a torn publication.
	ErrSectionMissing = errors.New("section named in manifest but absent from store")
)

// StaleError is the stale outcome, carrying the age and bound so the blocked
// point can report view_age_s. It matches ErrSectionStale with errors.Is.
type StaleError struct {
	Section string
	Age     time.Duration
	Bound   time.Duration
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("hub view for %s is %s old (bound %s)",
		e.Section, FormatAge(e.Age), FormatAge(e.Bound))
}

func (e *StaleError) Is(target error) bool { return target == ErrSectionStale }

// Reader reads the hub view on an edge.
type Reader struct {
	transport edge.Transport
	now       func() time.Time
	// AllowStale downgrades only the stale outcome to a return with
	// Provenance.Stale set. Every other blocked outcome still returns no bytes.
	AllowStale bool
}

// NewReader returns a Reader over transport.
func NewReader(transport edge.Transport) *Reader {
	return &Reader{transport: transport, now: time.Now}
}

// WithClock overrides the reader's clock; it exists for freshness tests.
func (r *Reader) WithClock(now func() time.Time) *Reader {
	r.now = now
	return r
}

// Manifest reads and validates the manifest, mapping every failure to one of
// the package's sentinel outcomes.
func (r *Reader) Manifest(ctx context.Context) (*Manifest, error) {
	data, err := r.transport.Get(ctx, ManifestKey)
	if errors.Is(err, edge.ErrNotFound) {
		return nil, ErrNoManifest
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStoreUnreachable, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrManifestUnreadable, err)
	}
	if manifest.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: %d (this build reads %d)",
			ErrUnknownSchema, manifest.SchemaVersion, SchemaVersion)
	}
	return &manifest, nil
}

// Section returns one section's bytes and its provenance, or one of the
// sentinel outcomes above with no bytes. staleAfter is the edge's freshness
// bound; a section whose stamp is older is ErrSectionStale unless the
// reader's AllowStale is set.
func (r *Reader) Section(ctx context.Context, name string, staleAfter time.Duration) ([]byte, Provenance, error) {
	manifest, err := r.Manifest(ctx)
	if err != nil {
		return nil, Provenance{}, err
	}
	stamp, ok := manifest.Sections[name]
	if !ok {
		if cause, failed := manifest.Errors[name]; failed {
			return nil, Provenance{}, fmt.Errorf("%w: the hub could not publish %s: %s",
				ErrSectionAbsent, name, cause)
		}
		return nil, Provenance{}, fmt.Errorf("%w: the hub has not published %s",
			ErrSectionAbsent, name)
	}
	now := r.now()
	age := now.Sub(stamp.PublishedAt)
	prov := Provenance{
		HubHost:     manifest.HubHost,
		PublishedAt: stamp.PublishedAt,
		Age:         age,
		AgeSeconds:  age.Seconds(),
		Transport:   r.transport.Name(),
	}
	if staleAfter > 0 && age > staleAfter {
		if !r.AllowStale {
			return nil, Provenance{}, &StaleError{Section: name, Age: age, Bound: staleAfter}
		}
		prov.Stale = true
	}
	body, err := r.transport.Get(ctx, SectionKey(name))
	if errors.Is(err, edge.ErrNotFound) {
		return nil, Provenance{}, fmt.Errorf("%w: %s", ErrSectionMissing, name)
	}
	if err != nil {
		return nil, Provenance{}, fmt.Errorf("%w: %v", ErrStoreUnreachable, err)
	}
	return body, prov, nil
}
