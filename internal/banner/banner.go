// Package banner is a small in-process pubsub for ambient system-status
// signals — schema drift, autopilot pause, cloud-provider health — that are
// useful to surface in any long-running TUI or watch loop, regardless of
// which one the user happens to be looking at.
//
// Producers (monitors) call Bus.Push(Banner{ID: ..., ...}) when their
// condition becomes true and Bus.Clear(id) when it goes away. Banners are
// keyed by ID so repeated pushes with the same ID replace the previous
// banner — this lets a polling monitor refresh the timestamp / hint / text
// without growing the list.
//
// Subscribers (TUIs, plain watchers) call Bus.Subscribe() to receive a
// stream of state changes, plus Bus.Snapshot() for the initial render.
//
// The Bus is concurrency-safe; producers and subscribers may run on
// independent goroutines.
package banner

import (
	"sort"
	"sync"
	"time"
)

// Severity controls how a banner is rendered (e.g. color in TUIs).
type Severity int

const (
	// SeverityInfo: informational, neutral color.
	SeverityInfo Severity = iota
	// SeverityWarning: visible attention but non-blocking (yellow / orange).
	SeverityWarning
	// SeverityCritical: high-attention, persistent (red).
	SeverityCritical
)

// Banner is a single ambient status notice.
type Banner struct {
	// ID is a stable identifier for this notice; pushing with the same ID
	// replaces the previous Banner. Use a constant per monitor (e.g.
	// "schema-drift", "r2-unreachable").
	ID string

	// Severity controls rendering.
	Severity Severity

	// Text is the primary one-line message. Keep it short.
	Text string

	// Hint is an optional follow-up suggestion ("press R to relaunch",
	// "stop other weft processes", etc.). May be empty.
	Hint string

	// Detected is when the producer first observed the condition. Set
	// automatically by Bus.Push if zero.
	Detected time.Time
}

// Event is published to subscribers when a banner is added, replaced, or
// cleared.
type Event struct {
	// Banner is the new or replaced banner. For Cleared events, only ID
	// is meaningful.
	Banner Banner
	// Cleared is true when a banner with Banner.ID was removed via
	// Bus.Clear; otherwise this is an add/replace event.
	Cleared bool
}

// Bus is the central pubsub hub.
type Bus struct {
	mu      sync.Mutex
	banners map[string]Banner
	subs    map[chan Event]struct{}
}

// NewBus creates an empty Bus.
func NewBus() *Bus {
	return &Bus{
		banners: map[string]Banner{},
		subs:    map[chan Event]struct{}{},
	}
}

// Push adds or replaces a banner. If b.Detected is zero it is set to now.
// Sends a non-blocking Event to every subscriber; full subscriber channels
// drop the event (subscribers are expected to consume promptly).
func (b *Bus) Push(banner Banner) {
	if banner.ID == "" {
		return
	}
	if banner.Detected.IsZero() {
		banner.Detected = time.Now()
	}
	b.mu.Lock()
	prev, existed := b.banners[banner.ID]
	if existed && prev == banner {
		b.mu.Unlock()
		return
	}
	b.banners[banner.ID] = banner
	subs := b.snapshotSubsLocked()
	b.mu.Unlock()
	dispatch(subs, Event{Banner: banner})
}

// Clear removes the banner with the given ID, if any. Subscribers receive
// a Cleared event with the corresponding ID.
func (b *Bus) Clear(id string) {
	if id == "" {
		return
	}
	b.mu.Lock()
	prev, existed := b.banners[id]
	if !existed {
		b.mu.Unlock()
		return
	}
	delete(b.banners, id)
	subs := b.snapshotSubsLocked()
	b.mu.Unlock()
	dispatch(subs, Event{Banner: Banner{ID: prev.ID}, Cleared: true})
}

// Snapshot returns the current banner set, sorted by Detected (oldest
// first) so the rendered order is stable.
func (b *Bus) Snapshot() []Banner {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Banner, 0, len(b.banners))
	for _, banner := range b.banners {
		out = append(out, banner)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Detected.Before(out[j].Detected)
	})
	return out
}

// Subscribe returns a channel that receives Events and an unsubscribe
// function the caller must invoke (e.g. with defer) when done. The
// channel is buffered; if a slow consumer falls behind, events are
// dropped to avoid stalling producers.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	unsubscribe := func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
	return ch, unsubscribe
}

func (b *Bus) snapshotSubsLocked() []chan Event {
	subs := make([]chan Event, 0, len(b.subs))
	for ch := range b.subs {
		subs = append(subs, ch)
	}
	return subs
}

func dispatch(subs []chan Event, ev Event) {
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
			// Slow consumer: drop. Subscribers re-derive state from
			// Bus.Snapshot() on reconnect / next render if they need to
			// recover from a missed event.
		}
	}
}
