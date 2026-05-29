package dashtabs

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// CycleState controls automatic tab rotation. When enabled, the parent
// schedules a tea.Tick at Interval and advances the active tab when it fires.
//
// The user pressing a tab key resets NextSwitchAt so they don't get yanked
// off the tab they just selected.
type CycleState struct {
	Enabled      bool
	Interval     time.Duration
	NextSwitchAt time.Time
}

// cycleIntervals is the ladder we step through with +/-.
var cycleIntervals = []time.Duration{
	5 * time.Second,
	10 * time.Second,
	15 * time.Second,
	20 * time.Second,
	30 * time.Second,
	45 * time.Second,
	60 * time.Second,
	90 * time.Second,
	120 * time.Second,
}

// defaultCycleInterval is what we use when cycle is first enabled without
// an explicit --cycle flag.
const defaultCycleInterval = 15 * time.Second

type cycleTickMsg time.Time

func (c CycleState) tick() tea.Cmd {
	if !c.Enabled {
		return nil
	}
	d := time.Until(c.NextSwitchAt)
	if d <= 0 {
		d = c.Interval
	}
	return tea.Tick(d, func(t time.Time) tea.Msg { return cycleTickMsg(t) })
}

func stepFaster(d time.Duration) time.Duration {
	for i, v := range cycleIntervals {
		if v == d && i > 0 {
			return cycleIntervals[i-1]
		}
	}
	return cycleIntervals[0]
}

func stepSlower(d time.Duration) time.Duration {
	for i, v := range cycleIntervals {
		if v == d && i+1 < len(cycleIntervals) {
			return cycleIntervals[i+1]
		}
	}
	return cycleIntervals[len(cycleIntervals)-1]
}
