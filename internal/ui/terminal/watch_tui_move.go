package terminal

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/orchestration"
	dashboard "github.com/osteele/weft/internal/ui/dashboard"
)

// moveOption represents a destination for moving a queued job.
type moveOption struct {
	kind        orchestration.OptionKind
	isNew       bool                      // true = launch new instance; false = move to existing
	host        string                    // on-prem host (on-prem only)
	instanceID  int64                     // DB launch ID (existing only)
	offer       *cloud.Offer              // cloud offer (new only)
	strategy    bidding.SelectionStrategy // "cheap"/"fast"/"fastest" (new only)
	waitTime    time.Duration             // estimated wait (existing) or setup overhead (new)
	queueDepth  int
	cpuPercent  int
	hasCPULoad  bool
	gpuPercent  int
	hasGPULoad  bool
	costPerHour float64 // $/hr
	gpuName     string  // resolved GPU name for display
	eligible    bool
	reason      string
}

// movePickerModel is an inline overlay for selecting a move destination.
type movePickerModel struct {
	active       bool
	jobID        int64
	requestID    int64
	options      []moveOption
	cursor       int
	loadingNew   bool
	loadingStart time.Time
	loadingFrame int
	status       string
	existingDone bool
}

func (p *movePickerModel) reset() {
	p.active = false
	p.jobID = 0
	p.options = nil
	p.cursor = 0
	p.loadingStart = time.Time{}
	p.loadingFrame = 0
}

func (p *movePickerModel) selectableCount() int {
	count := 0
	for _, opt := range p.options {
		if opt.eligible {
			count++
		}
	}
	return count
}

func (p *movePickerModel) moveCursor(delta int) {
	if p.selectableCount() == 0 {
		return
	}
	p.cursor += delta
	if p.cursor < 0 || p.cursor >= len(p.options) {
		if delta >= 0 {
			p.cursor = firstEligibleMoveOption(p.options)
		} else {
			p.cursor = lastEligibleMoveOption(p.options)
		}
		return
	}
	p.cursor = nearestEligibleMoveOption(p.options, p.cursor, delta)
}

func (p *movePickerModel) selectedOption() *moveOption {
	if p.cursor >= 0 && p.cursor < len(p.options) && p.options[p.cursor].eligible {
		return &p.options[p.cursor]
	}
	return nil
}

func (p *movePickerModel) addNewOptions(options []moveOption) {
	p.loadingNew = false
	p.options = append(p.options, options...)
	if p.cursor < 0 || p.cursor >= len(p.options) || !p.options[p.cursor].eligible {
		p.cursor = firstEligibleMoveOption(p.options)
	}
}

func (p *movePickerModel) setExistingOptions(options []moveOption) {
	newOptions := make([]moveOption, 0)
	for _, opt := range p.options {
		if opt.kind == orchestration.OptionKindNew {
			newOptions = append(newOptions, opt)
		}
	}
	p.options = append(append([]moveOption{}, options...), newOptions...)
	if p.cursor < 0 || p.cursor >= len(p.options) || !p.options[p.cursor].eligible {
		p.cursor = firstEligibleMoveOption(p.options)
	}
}

func firstEligibleMoveOption(options []moveOption) int {
	for i, opt := range options {
		if opt.eligible {
			return i
		}
	}
	return 0
}

func lastEligibleMoveOption(options []moveOption) int {
	for i := len(options) - 1; i >= 0; i-- {
		if options[i].eligible {
			return i
		}
	}
	return 0
}

func nearestEligibleMoveOption(options []moveOption, start int, delta int) int {
	if len(options) == 0 {
		return 0
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	for i := start; i >= 0 && i < len(options); i += step {
		if options[i].eligible {
			return i
		}
	}
	if step > 0 {
		return lastEligibleMoveOption(options)
	}
	return firstEligibleMoveOption(options)
}

var (
	movePickerBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(tuiAccentColor).
				Padding(1, 2)
	movePickerTitleStyle    = tuiTitleStyle
	movePickerSectionStyle  = tuiDimStyle
	movePickerSelectedStyle = lipgloss.NewStyle().Background(lipgloss.Color("62")).Foreground(lipgloss.Color("230"))
	movePickerDimStyle      = tuiDimStyle
	movePickerDisabledStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Faint(true)
	movePickerBackdropStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

func (p *movePickerModel) content() string {
	if !p.active {
		return ""
	}

	var b strings.Builder
	b.WriteString(movePickerTitleStyle.Render(fmt.Sprintf("Move job #%d to:", p.jobID)))
	b.WriteString("\n")
	status := p.status
	if p.isLoading() {
		status = "Searching move destinations " + p.loadingElapsedText()
	}
	if strings.TrimSpace(status) != "" {
		b.WriteString("\n")
		b.WriteString(movePickerDimStyle.Render(status))
		b.WriteString("\n")
	}

	// Separate existing destinations (on-prem hosts and running rentals) from new rentals.
	hasExisting := false
	hasNew := false
	newCount := 0
	for _, o := range p.options {
		switch o.kind {
		case orchestration.OptionKindOnPrem:
			hasExisting = true
		case orchestration.OptionKindNew:
			hasNew = true
			newCount++
		default:
			hasExisting = true
		}
	}

	if hasExisting {
		b.WriteString("\n")
		b.WriteString(movePickerSectionStyle.Render("EXISTING INSTANCES"))
		b.WriteString("\n")
		for i, o := range p.options {
			if o.kind == orchestration.OptionKindNew {
				continue
			}
			b.WriteString(renderMoveOptionLine(o, i == p.cursor))
			b.WriteString("\n")
		}
	} else if !p.existingDone {
		b.WriteString("\n")
		b.WriteString(movePickerSectionStyle.Render("EXISTING INSTANCES"))
		b.WriteString("\n")
		b.WriteString(movePickerDimStyle.Render("  searching destinations..."))
		b.WriteString("\n")
	} else if p.existingDone {
		b.WriteString("\n")
		b.WriteString(movePickerSectionStyle.Render("EXISTING INSTANCES"))
		b.WriteString("\n")
		b.WriteString(movePickerDimStyle.Render("  none available"))
		b.WriteString("\n")
	}

	if hasNew || p.loadingNew {
		b.WriteString("\n")
		b.WriteString(movePickerSectionStyle.Render("NEW INSTANCE"))
		b.WriteString("\n")
		for i, o := range p.options {
			if o.kind != orchestration.OptionKindNew {
				continue
			}
			b.WriteString(renderMoveOptionLine(o, i == p.cursor, newCount > 1))
			b.WriteString("\n")
		}
		if p.loadingNew {
			b.WriteString(movePickerDimStyle.Render("  waiting for cloud offers..."))
			b.WriteString("\n")
		}
	}

	if len(p.options) == 0 && !p.loadingNew {
		b.WriteString("\n")
		b.WriteString(movePickerDimStyle.Render("No compatible destinations found."))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(movePickerDimStyle.Render("[↑↓] select  [enter] confirm  [esc] cancel"))

	return b.String()
}

func (p *movePickerModel) isLoading() bool {
	return p.active && p.loadingNew && !p.existingDone && len(p.options) == 0
}

func (p *movePickerModel) loadingElapsedText() string {
	if p.loadingStart.IsZero() {
		return "(...)"
	}
	elapsed := time.Since(p.loadingStart).Truncate(time.Second)
	return fmt.Sprintf("(%s elapsed)", dashboard.FormatCompactDuration(elapsed))
}

// View renders the move picker overlay.
func (p *movePickerModel) View(width, height int) string {
	content := movePickerBorderStyle.Render(p.content())

	// Center the overlay
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func (p *movePickerModel) ViewOver(width, height int, background string) string {
	content := movePickerBorderStyle.Render(p.content())
	if !p.active || width <= 0 || height <= 0 {
		return p.View(width, height)
	}
	return placeMovePickerOverlay(dimMovePickerBackdrop(background, width, height), content, width, height)
}

func dimMovePickerBackdrop(background string, width, height int) []string {
	lines := strings.Split(ansi.Strip(background), "\n")
	out := make([]string, height)
	for i := 0; i < height; i++ {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		out[i] = movePickerBackdropStyle.Render(padOrTruncateDisplay(line, width, false))
	}
	return out
}

func placeMovePickerOverlay(background []string, overlay string, width, height int) string {
	overlayLines := strings.Split(overlay, "\n")
	overlayHeight := len(overlayLines)
	overlayWidth := 0
	for _, line := range overlayLines {
		overlayWidth = max(overlayWidth, lipgloss.Width(line))
	}
	x := max(0, (width-overlayWidth)/2)
	y := max(0, (height-overlayHeight)/2)
	for i, overlayLine := range overlayLines {
		row := y + i
		if row < 0 || row >= len(background) {
			continue
		}
		plain := ansi.Strip(background[row])
		left := displayPrefixWidth(plain, x)
		rightStart := min(lipgloss.Width(left)+overlayWidth, width)
		right := ""
		if rightStart < width {
			right = displaySubstringFromWidth(plain, rightStart)
			right = truncateDisplayWidth(right, width-rightStart)
		}
		background[row] = left + overlayLine + right
	}
	return strings.Join(background, "\n")
}

func displayPrefixWidth(s string, widthLimit int) string {
	if widthLimit <= 0 {
		return ""
	}
	var b strings.Builder
	width := 0
	for _, r := range s {
		nextWidth := width + lipgloss.Width(string(r))
		if nextWidth > widthLimit {
			break
		}
		b.WriteRune(r)
		width = nextWidth
	}
	if pad := widthLimit - width; pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	return b.String()
}

func displaySubstringFromWidth(s string, start int) string {
	if start <= 0 {
		return s
	}
	width := 0
	for idx, r := range s {
		nextWidth := width + lipgloss.Width(string(r))
		if nextWidth > start {
			return s[idx:]
		}
		width = nextWidth
	}
	return ""
}

func renderMoveOptionLine(o moveOption, selected bool, showNewStrategy ...bool) string {
	showStrategy := true
	if len(showNewStrategy) > 0 {
		showStrategy = showNewStrategy[0]
	}
	line := formatMoveOptionLineWithStrategy(o, showStrategy)
	if !o.eligible {
		return movePickerDisabledStyle.Render(line)
	}
	if selected {
		return movePickerSelectedStyle.Render(line)
	}
	return line
}

func formatMoveOptionLine(o moveOption) string {
	return formatMoveOptionLineWithStrategy(o, true)
}

func formatMoveOptionLineWithStrategy(o moveOption, showNewStrategy bool) string {
	var line string
	switch o.kind {
	case orchestration.OptionKindOnPrem:
		trailing := "wait ~" + dashboard.FormatCompactDuration(o.waitTime)
		if !o.eligible && strings.TrimSpace(o.reason) != "" {
			trailing = strings.TrimSpace(o.reason)
		}
		line = fmt.Sprintf("  %s %-12s queue %-2d %-8s %-8s %s",
			formatMovePickerTargetLabel("⌂", o.host, 13), formatMovePickerGPUName(o.gpuName, 12), o.queueDepth, formatMovePickerCPU(o), formatMovePickerGPU(o), trailing)
	case orchestration.OptionKindNew:
		if showNewStrategy {
			line = fmt.Sprintf("  %-13s %-14s %-10s $%.2f/hr",
				string(o.strategy), o.gpuName, "~"+dashboard.FormatCompactDuration(o.waitTime)+" launch", o.costPerHour)
		} else {
			line = fmt.Sprintf("  %-14s %-10s $%.2f/hr",
				o.gpuName, "~"+dashboard.FormatCompactDuration(o.waitTime)+" launch", o.costPerHour)
		}
	default:
		line = fmt.Sprintf("  %s %-14s queue %-2d wait ~%s  $%.2f/hr",
			formatMovePickerTargetLabel(" ", ids.FormatInstanceID(o.instanceID), 13), o.gpuName, o.queueDepth, dashboard.FormatCompactDuration(o.waitTime), o.costPerHour)
	}
	if o.kind != orchestration.OptionKindOnPrem && !o.eligible && strings.TrimSpace(o.reason) != "" {
		line += "  - " + strings.TrimSpace(o.reason)
	}
	return line
}

func formatMovePickerTargetLabel(prefix, label string, width int) string {
	return padOrTruncateDisplay(prefix+strings.TrimSpace(label), width, false)
}

func formatMovePickerGPUName(name string, width int) string {
	name = strings.TrimSpace(name)
	replacer := strings.NewReplacer(
		"NVIDIA GeForce ", "",
		"NVIDIA ", "",
		"GeForce ", "",
		"Tesla ", "",
	)
	name = replacer.Replace(name)
	if name == "" {
		name = "on-prem"
	}
	return truncateDisplayWidth(name, width)
}

func formatMovePickerCPU(o moveOption) string {
	if !o.hasCPULoad {
		return ""
	}
	return fmt.Sprintf("CPU %d%%", o.cpuPercent)
}

func formatMovePickerGPU(o moveOption) string {
	if !o.hasGPULoad {
		return ""
	}
	return fmt.Sprintf("GPU %d%%", o.gpuPercent)
}

func movePickerStatus(existingCount, newCount, disabledCount int, loading bool) string {
	switch {
	case loading && existingCount > 0:
		return fmt.Sprintf("%s ready; searching cloud offers...", movePickerDestinationCount(existingCount, "existing"))
	case loading:
		return "No existing destinations; searching cloud offers..."
	case existingCount > 0 || newCount > 0:
		return fmt.Sprintf("Found %s and %s.", movePickerDestinationCount(existingCount, "existing"), movePickerDestinationCount(newCount, "new"))
	default:
		return "No compatible destinations found."
	}
}

func movePickerDestinationCount(count int, label string) string {
	noun := "destinations"
	if count == 1 {
		noun = "destination"
	}
	return fmt.Sprintf("%d %s %s", count, label, noun)
}

func countMoveOptions(options []moveOption) (existingCount, newCount, disabledCount int) {
	for _, opt := range options {
		if !opt.eligible {
			disabledCount++
			continue
		}
		if opt.kind == orchestration.OptionKindNew || opt.isNew {
			newCount++
		} else {
			existingCount++
		}
	}
	return existingCount, newCount, disabledCount
}

func moveOptionsFromOrchestration(options []orchestration.Option) []moveOption {
	moveOptions := make([]moveOption, 0, len(options))
	for _, o := range options {
		moveOptions = append(moveOptions, moveOption{
			kind:        o.Kind,
			isNew:       o.IsNew,
			host:        o.Host,
			instanceID:  o.InstanceID,
			offer:       o.Offer,
			strategy:    o.Strategy,
			gpuName:     o.GPUName,
			waitTime:    o.WaitTime,
			queueDepth:  o.QueueDepth,
			cpuPercent:  o.CPUPercent,
			hasCPULoad:  o.HasCPULoad,
			gpuPercent:  o.GPUPercent,
			hasGPULoad:  o.HasGPULoad,
			costPerHour: o.CostPerHour,
			eligible:    o.Eligible,
			reason:      o.Reason,
		})
	}
	return moveOptions
}
