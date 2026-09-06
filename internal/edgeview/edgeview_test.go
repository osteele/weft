package edgeview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/edge"
)

// The freshness tests run against the filesystem transport in a temp dir —
// a real transport, not a mock — so the reader's outcome mapping is exercised
// against the same Get/Put semantics production uses.

func newTestTransport(t *testing.T) *edge.FSTransport {
	t.Helper()
	transport, err := edge.NewFSTransport(filepath.Join(t.TempDir(), "view"))
	if err != nil {
		t.Fatal(err)
	}
	return transport
}

func staticProducer(body string) Producer {
	return func(context.Context) ([]byte, error) { return []byte(body), nil }
}

func failingProducer(err error) Producer {
	return func(context.Context) ([]byte, error) { return nil, err }
}

func TestPublishWritesManifestLastAndStampsSections(t *testing.T) {
	transport := newTestTransport(t)
	pub := NewPublisher(transport, "hub-a", "digest-1")
	manifest, err := pub.Publish(context.Background(), map[string]Producer{
		SectionJobsIndex: staticProducer(`{"jobs":[]}`),
		SectionHosts:     staticProducer(`{"hosts":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", manifest.SchemaVersion, SchemaVersion)
	}
	if manifest.HubHost != "hub-a" || manifest.SourceDigest != "digest-1" {
		t.Fatalf("manifest identity = %q/%q", manifest.HubHost, manifest.SourceDigest)
	}
	for _, name := range []string{SectionJobsIndex, SectionHosts} {
		stamp, ok := manifest.Sections[name]
		if !ok {
			t.Fatalf("no stamp for %s", name)
		}
		if stamp.Bytes == 0 || stamp.PublishedAt.IsZero() {
			t.Fatalf("stamp for %s not populated: %+v", name, stamp)
		}
		if _, err := transport.Get(context.Background(), SectionKey(name)); err != nil {
			t.Fatalf("section %s not in store: %v", name, err)
		}
	}
}

func TestPublishSectionFailureIsRecordedNotFatal(t *testing.T) {
	transport := newTestTransport(t)
	pub := NewPublisher(transport, "hub-a", "")
	manifest, err := pub.Publish(context.Background(), map[string]Producer{
		SectionJobsIndex: staticProducer(`{"jobs":[]}`),
		SectionHosts:     failingProducer(errors.New("inventory read failed")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manifest.Sections[SectionJobsIndex]; !ok {
		t.Fatal("good section lost to a sibling's failure")
	}
	if _, ok := manifest.Sections[SectionHosts]; ok {
		t.Fatal("failed section got a stamp")
	}
	if got := manifest.Errors[SectionHosts]; got != "inventory read failed" {
		t.Fatalf("errors[%s] = %q", SectionHosts, got)
	}
	// A reader asking for the failed section learns the cause, not empty bytes.
	reader := NewReader(transport)
	body, _, err := reader.Section(context.Background(), SectionHosts, time.Minute)
	if !errors.Is(err, ErrSectionAbsent) {
		t.Fatalf("err = %v, want ErrSectionAbsent", err)
	}
	if body != nil {
		t.Fatal("absent section returned bytes")
	}
	if want := "inventory read failed"; !strings.Contains(err.Error(), want) {
		t.Fatalf("absent-section message %q lacks the hub's error %q", err, want)
	}
}

// TestRepeatedPublishRefreshesSectionBytesAndStamps kills a timestamp-only
// heartbeat mutation that leaves a growing log tail unchanged on the edge.
func TestRepeatedPublishRefreshesSectionBytesAndStamps(t *testing.T) {
	transport := newTestTransport(t)
	first := time.Date(2026, 9, 8, 1, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	producerCalls := 0
	pub := NewPublisher(transport, "hub-a", "digest-1")
	pub.now = func() time.Time { return first }
	_, err := pub.Publish(context.Background(), map[string]Producer{
		SectionJobsIndex: func(context.Context) ([]byte, error) {
			producerCalls++
			return []byte(`{"jobs":[]}`), nil
		},
		SectionHosts: failingProducer(errors.New("inventory unavailable")),
	})
	if err != nil {
		t.Fatal(err)
	}
	pub.now = func() time.Time { return second }
	heartbeat, err := pub.Publish(context.Background(), map[string]Producer{
		SectionJobsIndex: func(context.Context) ([]byte, error) {
			producerCalls++
			return []byte(`{"jobs":[{"id":42}]}`), nil
		},
		SectionHosts: failingProducer(errors.New("inventory unavailable")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if producerCalls != 2 {
		t.Fatalf("producer calls = %d, want 2", producerCalls)
	}
	if got := heartbeat.Sections[SectionJobsIndex].PublishedAt; !got.Equal(second) {
		t.Fatalf("section stamp = %s, want %s", got, second)
	}
	if got := heartbeat.Errors[SectionHosts]; got != "inventory unavailable" {
		t.Fatalf("failed section = %q", got)
	}
	body, err := transport.Get(context.Background(), SectionKey(SectionJobsIndex))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"jobs":[{"id":42}]}` {
		t.Fatalf("section body = %s", body)
	}
}

// Freshness table row: store unreachable.
func TestSectionStoreUnreachable(t *testing.T) {
	// Replace the transport root with a regular file after construction: every
	// read then fails with ENOTDIR — the store cannot be consulted, and the
	// failure is not a confirmed absence.
	root := filepath.Join(t.TempDir(), "view")
	transport, err := edge.NewFSTransport(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(transport)
	body, _, err := reader.Section(context.Background(), SectionJobsIndex, time.Minute)
	if !errors.Is(err, ErrStoreUnreachable) {
		t.Fatalf("err = %v, want ErrStoreUnreachable", err)
	}
	if body != nil {
		t.Fatal("unreachable store returned bytes")
	}
}

// Freshness table row: no manifest.
func TestSectionNoManifest(t *testing.T) {
	reader := NewReader(newTestTransport(t))
	body, _, err := reader.Section(context.Background(), SectionJobsIndex, time.Minute)
	if !errors.Is(err, ErrNoManifest) {
		t.Fatalf("err = %v, want ErrNoManifest", err)
	}
	if body != nil {
		t.Fatal("missing manifest returned bytes")
	}
}

func TestSectionUnknownSchemaRefused(t *testing.T) {
	transport := newTestTransport(t)
	if err := transport.Put(context.Background(), ManifestKey,
		[]byte(`{"schema_version": 999, "hub_host": "hub-a", "sections": {}}`)); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(transport)
	body, _, err := reader.Section(context.Background(), SectionJobsIndex, time.Minute)
	if !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("err = %v, want ErrUnknownSchema", err)
	}
	if body != nil {
		t.Fatal("unknown schema returned bytes")
	}
}

// Freshness table row: section absent from the manifest (never attempted).
func TestSectionAbsentWithoutError(t *testing.T) {
	transport := newTestTransport(t)
	pub := NewPublisher(transport, "hub-a", "")
	if _, err := pub.Publish(context.Background(), map[string]Producer{
		SectionJobsIndex: staticProducer(`{"jobs":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(transport)
	body, _, err := reader.Section(context.Background(), SectionAutopilot, time.Minute)
	if !errors.Is(err, ErrSectionAbsent) {
		t.Fatalf("err = %v, want ErrSectionAbsent", err)
	}
	if body != nil {
		t.Fatal("absent section returned bytes")
	}
}

// Freshness table row: section older than the bound.
func TestSectionStale(t *testing.T) {
	transport := newTestTransport(t)
	published := time.Now()
	pub := NewPublisher(transport, "hub-a", "")
	pub.now = func() time.Time { return published }
	if _, err := pub.Publish(context.Background(), map[string]Producer{
		SectionJobsIndex: staticProducer(`{"jobs":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(transport)
	reader.now = func() time.Time { return published.Add(47 * time.Minute) }
	body, _, err := reader.Section(context.Background(), SectionJobsIndex, 5*time.Minute)
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("err = %v, want *StaleError", err)
	}
	if body != nil {
		t.Fatal("stale section returned bytes")
	}
	if stale.Age != 47*time.Minute || stale.Bound != 5*time.Minute {
		t.Fatalf("stale carries age %s bound %s", stale.Age, stale.Bound)
	}
	if want := "hub view for jobs/index.json is 47m old (bound 5m)"; stale.Error() != want {
		t.Fatalf("message = %q, want %q", stale.Error(), want)
	}
}

// Freshness table row: fresh section, zero rows — a true empty, rendered.
func TestSectionFreshEmpty(t *testing.T) {
	transport := newTestTransport(t)
	pub := NewPublisher(transport, "hub-a", "")
	if _, err := pub.Publish(context.Background(), map[string]Producer{
		SectionJobsIndex: staticProducer(`{"jobs":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	body, prov, err := NewReader(transport).Section(context.Background(), SectionJobsIndex, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"jobs":[]}` {
		t.Fatalf("body = %q", body)
	}
	if prov.HubHost != "hub-a" || prov.Stale {
		t.Fatalf("provenance = %+v", prov)
	}
}

// Freshness table row: fresh section carries provenance.
func TestSectionFresh(t *testing.T) {
	transport := newTestTransport(t)
	published := time.Now()
	pub := NewPublisher(transport, "hub-a", "")
	pub.now = func() time.Time { return published }
	if _, err := pub.Publish(context.Background(), map[string]Producer{
		SectionJobsIndex: staticProducer(`{"jobs":[{"job_id":"wj1"}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(transport)
	reader.now = func() time.Time { return published.Add(12 * time.Second) }
	body, prov, err := reader.Section(context.Background(), SectionJobsIndex, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Fatal("no bytes")
	}
	if prov.Age != 12*time.Second || prov.Stale {
		t.Fatalf("provenance = %+v", prov)
	}
	if prov.Transport != transport.Name() {
		t.Fatalf("transport = %q, want %q", prov.Transport, transport.Name())
	}
	if want := fmt.Sprintf("source: hub hub-a via %s, published 12s ago", transport.Name()); prov.Line() != want {
		t.Fatalf("line = %q, want %q", prov.Line(), want)
	}
}

// --allow-stale downgrades only the stale outcome.
func TestSectionAllowStale(t *testing.T) {
	transport := newTestTransport(t)
	published := time.Now()
	pub := NewPublisher(transport, "hub-a", "")
	pub.now = func() time.Time { return published }
	if _, err := pub.Publish(context.Background(), map[string]Producer{
		SectionJobsIndex: staticProducer(`{"jobs":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(transport)
	reader.AllowStale = true
	reader.now = func() time.Time { return published.Add(time.Hour) }
	body, prov, err := reader.Section(context.Background(), SectionJobsIndex, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !prov.Stale || prov.Age != time.Hour {
		t.Fatalf("provenance = %+v, want stale with age 1h", prov)
	}
	if len(body) == 0 {
		t.Fatal("allow-stale returned no bytes")
	}
	// AllowStale does not downgrade absence.
	_, _, err = reader.Section(context.Background(), SectionHosts, 5*time.Minute)
	if !errors.Is(err, ErrSectionAbsent) {
		t.Fatalf("err = %v, want ErrSectionAbsent", err)
	}
}

// The manifest names a section whose object is gone: a torn publication is
// reported, never rendered as empty.
func TestSectionMissingObject(t *testing.T) {
	transport := newTestTransport(t)
	pub := NewPublisher(transport, "hub-a", "")
	if _, err := pub.Publish(context.Background(), map[string]Producer{
		SectionJobsIndex: staticProducer(`{"jobs":[]}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := transport.Delete(context.Background(), SectionKey(SectionJobsIndex)); err != nil {
		t.Fatal(err)
	}
	body, _, err := NewReader(transport).Section(context.Background(), SectionJobsIndex, time.Minute)
	if !errors.Is(err, ErrSectionMissing) {
		t.Fatalf("err = %v, want ErrSectionMissing", err)
	}
	if body != nil {
		t.Fatal("missing object returned bytes")
	}
}
