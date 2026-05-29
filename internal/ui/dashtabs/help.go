package dashtabs

import (
	"fmt"
	"strings"
)

// renderHelp draws the help overlay as a boxed pane. The parent decides where
// to position it; this just returns a string of the box.
func renderHelp(keys Keys, cycle CycleState, logPath string) string {
	lines := []string{
		titleStyle.Render("weft dashboard — keybindings"),
		"",
	}
	addRow := func(k, desc string) {
		lines = append(lines, fmt.Sprintf("  %-14s  %s", accentStyle.Render(k), desc))
	}

	addRow("1 … 9, 0", "switch to numbered tab (0 = Usage)")
	addRow("← / →", "prev / next tab on the same row (wraps)")
	addRow("↑ / ↓", "row above / below at nearest column (wraps)")
	addRow("Tab / ⇧Tab", "linear next / previous through all tabs")
	addRow("?", "toggle this help")
	addRow("c", fmt.Sprintf("toggle cycle mode (currently %s)", cycleStateStr(cycle)))
	addRow("+ / -", "faster / slower cycle interval")
	addRow("p", "pause/resume autopilot")
	addRow("r", "force refresh")
	addRow("q / ctrl-c", "quit")
	addRow("ctrl-z", "suspend (fg to resume)")

	lines = append(lines, "")
	lines = append(lines, dimStyle.Render("Read-only except for autopilot pause/resume."))
	if logPath != "" {
		lines = append(lines, dimStyle.Render("Usage log: "+logPath))
	}

	body := strings.Join(lines, "\n")
	return helpBoxStyle.Render(body)
}

func cycleStateStr(c CycleState) string {
	if !c.Enabled {
		return "off"
	}
	return "on @ " + c.Interval.String()
}
