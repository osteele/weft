package dashtabs

import "github.com/charmbracelet/bubbles/key"

// Keys is the set of keybindings used by the dashboard. Each tab can shadow
// these for its own actions; the parent dispatches to the active view first
// (for keys the view claims) and otherwise applies its own bindings.
type Keys struct {
	Quit        key.Binding
	Suspend     key.Binding
	NextTab     key.Binding // Tab — linear advance through all tabs
	PrevTab     key.Binding // ⇧Tab — linear advance backwards
	RightInRow  key.Binding // → — next tab on the same row (with wrap)
	LeftInRow   key.Binding // ← — prev tab on the same row (with wrap)
	DownRow     key.Binding // ↓ — next row, nearest-column match (with wrap)
	UpRow       key.Binding // ↑ — prev row, nearest-column match (with wrap)
	Tab1        key.Binding
	Tab2        key.Binding
	Tab3        key.Binding
	Tab4        key.Binding
	Tab5        key.Binding
	Tab6        key.Binding
	Tab7        key.Binding
	Tab8        key.Binding
	Tab9        key.Binding
	Tab10       key.Binding
	Help        key.Binding
	CloseHelp   key.Binding
	Cycle       key.Binding
	CycleFaster key.Binding
	CycleSlower key.Binding
	Autopilot   key.Binding
	Refresh     key.Binding
	Unprocessed key.Binding // 'U' — toggle "show only unprocessed jobs"
	LaunchTUI   key.Binding // 'L' — spawn `weft tui` (list view)
}

func defaultKeys() Keys {
	return Keys{
		Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Suspend:     key.NewBinding(key.WithKeys("ctrl+z"), key.WithHelp("ctrl-z", "suspend")),
		NextTab:     key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next tab")),
		PrevTab:     key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("⇧tab", "prev tab")),
		RightInRow:  key.NewBinding(key.WithKeys("right"), key.WithHelp("→", "next in row")),
		LeftInRow:   key.NewBinding(key.WithKeys("left"), key.WithHelp("←", "prev in row")),
		DownRow:     key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "row below")),
		UpRow:       key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "row above")),
		Tab1:        key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "Pulse")),
		Tab2:        key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "Timeline")),
		Tab3:        key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "Fleet")),
		Tab4:        key.NewBinding(key.WithKeys("4"), key.WithHelp("4", "Focus")),
		Tab5:        key.NewBinding(key.WithKeys("5"), key.WithHelp("5", "Tree")),
		Tab6:        key.NewBinding(key.WithKeys("6"), key.WithHelp("6", "Alerts")),
		Tab7:        key.NewBinding(key.WithKeys("7"), key.WithHelp("7", "History")),
		Tab8:        key.NewBinding(key.WithKeys("8"), key.WithHelp("8", "Flow")),
		Tab9:        key.NewBinding(key.WithKeys("9"), key.WithHelp("9", "Cost")),
		Tab10:       key.NewBinding(key.WithKeys("0"), key.WithHelp("0", "Usage")),
		Help:        key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		CloseHelp:   key.NewBinding(key.WithKeys("?", "esc"), key.WithHelp("?/esc", "close help")),
		Cycle:       key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "toggle cycle")),
		CycleFaster: key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+", "faster cycle")),
		CycleSlower: key.NewBinding(key.WithKeys("-", "_"), key.WithHelp("-", "slower cycle")),
		Autopilot:   key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause/resume autopilot")),
		Refresh:     key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "force refresh")),
		Unprocessed: key.NewBinding(key.WithKeys("U"), key.WithHelp("U", "toggle unprocessed-only filter")),
		LaunchTUI:   key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "open weft tui")),
	}
}
