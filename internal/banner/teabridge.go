package banner

import (
	tea "github.com/charmbracelet/bubbletea"
)

// TeaMsg is the bubbletea message type emitted for each Bus event. TUIs
// add a case for this in their Update method to maintain a per-program
// banner list (e.g. via a small View helper) and re-render.
type TeaMsg Event

// SubscribeCmd returns a tea.Cmd that subscribes to bus and reads one
// event, sending it back to the program as a TeaMsg. The Update handler
// must return another SubscribeCmd(bus, sub) to receive the next event;
// the same sub channel and unsubscribe are reused across calls.
//
// Typical wiring in a tea.Model:
//
//	func (m model) Init() tea.Cmd {
//	    sub, unsub := bus.Subscribe()
//	    m.bannerSub, m.bannerUnsub = sub, unsub
//	    return banner.SubscribeNext(sub)
//	}
//	func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
//	    switch msg := msg.(type) {
//	    case banner.TeaMsg:
//	        m.banners.Apply(banner.Event(msg))
//	        return m, banner.SubscribeNext(m.bannerSub)
//	    ...
//	    }
//	}
//	// On exit, call m.bannerUnsub().
func SubscribeNext(sub <-chan Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-sub
		if !ok {
			return nil
		}
		return TeaMsg(ev)
	}
}

// View tracks the current banner set inside a TUI model and renders it.
// Call Apply(event) for each TeaMsg; Render(width) returns the styled
// view (empty string if no banners).
type View struct {
	banners map[string]Banner
}

// NewView returns a View seeded with the given snapshot (typically
// bus.Snapshot() taken at Init time, so the first frame already shows
// any banners that existed before the TUI started).
func NewView(initial []Banner) *View {
	v := &View{banners: make(map[string]Banner, len(initial))}
	for _, b := range initial {
		v.banners[b.ID] = b
	}
	return v
}

// Apply incorporates an event into the View.
func (v *View) Apply(ev Event) {
	if ev.Cleared {
		delete(v.banners, ev.Banner.ID)
		return
	}
	v.banners[ev.Banner.ID] = ev.Banner
}

// Banners returns the current set, sorted by Detected (oldest first).
func (v *View) Banners() []Banner {
	out := make([]Banner, 0, len(v.banners))
	for _, b := range v.banners {
		out = append(out, b)
	}
	sortByDetected(out)
	return out
}

// Render returns the styled view (empty string if no banners).
func (v *View) Render() string {
	return Render(v.Banners())
}

// HasAny reports whether the view currently holds any banner.
func (v *View) HasAny() bool {
	return len(v.banners) > 0
}

func sortByDetected(out []Banner) {
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Detected.After(out[j].Detected); j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
}
