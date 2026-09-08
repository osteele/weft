package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/osteele/weft/internal/inventoryqueue"
)

// errStateStore fails every GetObject with a fixed error; PutObject records
// into a map that is never read.
type errStateStore struct {
	err error
}

func (s *errStateStore) GetObject(context.Context, string) ([]byte, error) {
	return nil, s.err
}

func (s *errStateStore) PutObject(_ context.Context, _ string, _ io.Reader, _ string) error {
	return nil
}

func withInventoryStateStore(t *testing.T, store inventoryQueueStore) {
	t.Helper()

	original := newInventoryQueueStore
	newInventoryQueueStore = func() (inventoryQueueStore, error) { return store, nil }
	t.Cleanup(func() { newInventoryQueueStore = original })
}

func publishedStateStore(t *testing.T, host string, updatedAt time.Time, agentVersion string) *fakeInventoryQueueStore {
	t.Helper()

	key, err := inventoryqueue.StateKey(host)
	if err != nil {
		t.Fatalf("StateKey: %v", err)
	}
	encoded, err := json.Marshal(inventoryqueue.State{
		Version: inventoryqueue.Version, Host: host,
		AgentVersion: agentVersion, UpdatedAt: updatedAt,
	})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return &fakeInventoryQueueStore{objects: map[string][]byte{key: encoded}}
}

func TestFetchR2StateViewAbsent(t *testing.T) {
	withInventoryStateStore(t, &fakeInventoryQueueStore{objects: map[string][]byte{}})

	view, err := FetchR2StateView("studio")
	if err != nil {
		t.Fatalf("absent state returned error: %v", err)
	}
	if view == nil || !view.Absent || view.State != nil {
		t.Fatalf("absent state view = %+v, want Absent with nil State", view)
	}
}

func TestFetchR2StateViewNotFoundObjectIsAbsentNotUnknown(t *testing.T) {
	withInventoryStateStore(t, &errStateStore{
		err: fmt.Errorf("get object inventory/v1/hosts/studio/state.json: %w", &types.NotFound{}),
	})

	view, err := FetchR2StateView("studio")
	if err != nil {
		t.Fatalf("not-found state returned error: %v", err)
	}
	if view == nil || !view.Absent {
		t.Fatalf("not-found state view = %+v, want Absent", view)
	}
}

func TestFetchR2StateViewFreshKnownVersion(t *testing.T) {
	withInventoryStateStore(t, publishedStateStore(t, "studio", time.Now(), "abc123def456"))

	view, err := FetchR2StateView("studio")
	if err != nil {
		t.Fatalf("FetchR2StateView: %v", err)
	}
	if view.Absent || view.State == nil {
		t.Fatalf("view = %+v, want decoded state", view)
	}
	if view.State.AgentVersion != "abc123def456" {
		t.Errorf("AgentVersion = %q, want %q", view.State.AgentVersion, "abc123def456")
	}
	if view.Stale {
		t.Error("fresh envelope reported stale")
	}
}

func TestFetchR2StateViewStaleEnvelopeStaysReadable(t *testing.T) {
	// The view exists for display surfaces: a stale envelope degrades to
	// stale-with-age and must keep its payload, unlike the placement
	// fetch that discards it.
	withInventoryStateStore(t, publishedStateStore(t, "studio", time.Now().Add(-2*inventoryStateMaxAge), "abc123def456"))

	view, err := FetchR2StateView("studio")
	if err != nil {
		t.Fatalf("stale state returned error: %v", err)
	}
	if view.Stale != true {
		t.Errorf("Stale = false for an aged envelope")
	}
	if view.State == nil || view.State.AgentVersion != "abc123def456" {
		t.Fatalf("stale view lost its payload: %+v", view)
	}
}

func TestFetchR2StateViewUndecodableIsUnknown(t *testing.T) {
	key, _ := inventoryqueue.StateKey("studio")
	withInventoryStateStore(t, &fakeInventoryQueueStore{
		objects: map[string][]byte{key: []byte("{not json")},
	})

	view, err := FetchR2StateView("studio")
	if err == nil {
		t.Fatalf("undecodable envelope returned view %+v, want error", view)
	}
	if view != nil {
		t.Errorf("undecodable envelope returned a view; absence and unknown must stay distinct")
	}
}

func TestFetchR2StateViewHostMismatchIsUnknown(t *testing.T) {
	// The object sits under host-beta's key but the envelope inside is
	// addressed to studio: unreadable for this caller, never absent.
	key, err := inventoryqueue.StateKey("host-beta")
	if err != nil {
		t.Fatalf("StateKey: %v", err)
	}
	encoded, err := json.Marshal(inventoryqueue.State{
		Version: inventoryqueue.Version, Host: "studio",
		AgentVersion: "abc123def456", UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	withInventoryStateStore(t, &fakeInventoryQueueStore{
		objects: map[string][]byte{key: encoded},
	})

	view, err := FetchR2StateView("host-beta")
	if err == nil {
		t.Fatalf("mismatched envelope returned view %+v, want error", view)
	}
	if view != nil {
		t.Errorf("mismatched envelope returned a view; absence and unknown must stay distinct")
	}
}
