package banner

import (
	"testing"
	"time"
)

func TestBus_PushSnapshotClear(t *testing.T) {
	bus := NewBus()

	bus.Push(Banner{ID: "a", Severity: SeverityWarning, Text: "alpha"})
	bus.Push(Banner{ID: "b", Severity: SeverityCritical, Text: "beta"})

	snap := bus.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	for _, b := range snap {
		if b.Detected.IsZero() {
			t.Fatalf("Detected was not auto-populated for %s", b.ID)
		}
	}

	bus.Clear("a")
	snap = bus.Snapshot()
	if len(snap) != 1 || snap[0].ID != "b" {
		t.Fatalf("after Clear, snapshot = %+v, want [b]", snap)
	}
}

func TestBus_PushReplacesById(t *testing.T) {
	bus := NewBus()
	bus.Push(Banner{ID: "a", Text: "old"})
	bus.Push(Banner{ID: "a", Text: "new"})
	snap := bus.Snapshot()
	if len(snap) != 1 || snap[0].Text != "new" {
		t.Fatalf("snap = %+v, want [a/new]", snap)
	}
}

func TestBus_SubscribeReceivesAllEvents(t *testing.T) {
	bus := NewBus()
	sub, unsub := bus.Subscribe()
	defer unsub()

	bus.Push(Banner{ID: "a", Text: "alpha"})
	bus.Push(Banner{ID: "b", Text: "beta"})
	bus.Clear("a")

	got := drainEvents(t, sub, 3, 200*time.Millisecond)
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	if got[0].Banner.ID != "a" || got[0].Cleared {
		t.Fatalf("event[0] = %+v, want add a", got[0])
	}
	if got[1].Banner.ID != "b" || got[1].Cleared {
		t.Fatalf("event[1] = %+v, want add b", got[1])
	}
	if got[2].Banner.ID != "a" || !got[2].Cleared {
		t.Fatalf("event[2] = %+v, want clear a", got[2])
	}
}

func TestBus_RepeatPushSameValueIsNoop(t *testing.T) {
	bus := NewBus()
	bus.Push(Banner{ID: "a", Text: "alpha", Detected: time.Unix(1000, 0)})
	sub, unsub := bus.Subscribe()
	defer unsub()
	// Identical re-push should not generate an event.
	bus.Push(Banner{ID: "a", Text: "alpha", Detected: time.Unix(1000, 0)})
	select {
	case ev := <-sub:
		t.Fatalf("unexpected event from no-op repush: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBus_UnsubscribeStopsDelivery(t *testing.T) {
	bus := NewBus()
	sub, unsub := bus.Subscribe()
	unsub()
	bus.Push(Banner{ID: "a", Text: "alpha"})
	select {
	case _, ok := <-sub:
		if ok {
			t.Fatal("received event after unsubscribe")
		}
	case <-time.After(50 * time.Millisecond):
		// Channel was closed by unsub; receiving the zero value is fine.
	}
}

func TestView_ApplyTracksAddsAndClears(t *testing.T) {
	v := NewView([]Banner{{ID: "seed", Text: "from snapshot", Detected: time.Unix(100, 0)}})
	if !v.HasAny() {
		t.Fatal("View seeded from snapshot should have a banner")
	}
	v.Apply(Event{Banner: Banner{ID: "added", Text: "new", Detected: time.Unix(200, 0)}})
	v.Apply(Event{Banner: Banner{ID: "seed"}, Cleared: true})

	got := v.Banners()
	if len(got) != 1 || got[0].ID != "added" {
		t.Fatalf("View.Banners = %+v, want [added]", got)
	}
}

func drainEvents(t *testing.T, sub <-chan Event, want int, timeout time.Duration) []Event {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	out := make([]Event, 0, want)
	for len(out) < want {
		select {
		case ev := <-sub:
			out = append(out, ev)
		case <-deadline.C:
			return out
		}
	}
	return out
}
