package terminal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/queueblock"
)

type jobListLayout struct {
	width    int
	columns  []columnDef
	cordoned cordonedTargets
}

type cordonedTargets struct {
	launches        map[int64]bool
	hosts           map[string]bool
	overloadedHosts map[string]bool
}

func (c cordonedTargets) IsCordoned(job *db.Job) bool {
	if job == nil {
		return false
	}
	switch job.TargetKind() {
	case db.JobTargetRentalInstance:
		return job.LaunchID != nil && c.launches[*job.LaunchID]
	case db.JobTargetInventoryHost:
		return c.hosts[job.Host]
	}
	return false
}

func (c cordonedTargets) IsOverloaded(job *db.Job) bool {
	if job == nil || job.TargetKind() != db.JobTargetInventoryHost {
		return false
	}
	return c.overloadedHosts[strings.TrimSpace(job.Host)]
}

const cordonMarker = "⊘"
const overloadMarker = "!"

func renderJobListPlain(jobs []*db.Job, width int) string {
	return renderJobListPlainWithOptions(jobs, width, nil, false)
}

func renderJobListPlainWithOptions(jobs []*db.Job, width int, columnKeys []string, noTruncate bool) string {
	if len(jobs) == 0 {
		return "No jobs found\n"
	}

	layout := newJobListLayout(width, jobs, columnKeys, noTruncate)
	lines := make([]string, 0, len(jobs)+1)
	if noTruncate {
		lines = append(lines, formatJobListHeader(layout))
		for _, job := range jobs {
			lines = append(lines, formatJobListRow(layout, job))
		}
	} else {
		lines = append(lines, truncateDisplayWidth(formatJobListHeader(layout), layout.width))
		for _, job := range jobs {
			lines = append(lines, truncateDisplayWidth(formatJobListRow(layout, job), layout.width))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func writeListPlainOutput(output string) error {
	if !term.IsTerminal(os.Stdout.Fd()) {
		_, err := io.WriteString(os.Stdout, output)
		return err
	}

	height := listOutputHeight()
	if height <= 0 || strings.Count(output, "\n") <= height-1 {
		_, err := io.WriteString(os.Stdout, output)
		return err
	}

	if err := pipeOutputThroughPager(output); err != nil {
		_, writeErr := io.WriteString(os.Stdout, output)
		if writeErr != nil {
			return writeErr
		}
	}
	return nil
}

func listOutputWidth() int {
	width, _, err := term.GetSize(os.Stdout.Fd())
	if err != nil || width <= 0 {
		return 120
	}
	return width
}

func listOutputHeight() int {
	_, height, err := term.GetSize(os.Stdout.Fd())
	if err != nil || height <= 0 {
		return 0
	}
	return height
}

func pipeOutputThroughPager(output string) error {
	cmd := pagerCommand()
	if cmd == nil {
		return fmt.Errorf("no pager configured")
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	_, writeErr := io.WriteString(stdin, output)
	closeErr := stdin.Close()
	waitErr := cmd.Wait()

	if ignorePagerPipeError(writeErr) || ignorePagerPipeError(waitErr) {
		return nil
	}
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return waitErr
}

func pagerCommand() *exec.Cmd {
	if pager := strings.TrimSpace(os.Getenv("PAGER")); pager != "" {
		return exec.Command("sh", "-c", pager)
	}
	return exec.Command("less", "-FRX")
}

func ignorePagerPipeError(err error) bool {
	return errors.Is(err, syscall.EPIPE)
}

// responsiveColumnKeys returns the column keys for the default responsive
// layout based on terminal width.
func responsiveColumnKeys(width int) []string {
	switch {
	case width >= 96:
		return []string{"check", "id", "host", "status", "started", "project", "description"}
	case width >= 80:
		return []string{"check", "id", "host", "status", "started", "project", "description"}
	case width >= 64:
		return []string{"check", "id", "host", "status", "project", "description"}
	default:
		return []string{"check", "id", "project", "status", "description"}
	}
}

func newJobListLayout(width int, jobs []*db.Job, columnKeys []string, noTruncate bool) jobListLayout {
	return newJobListLayoutWithCordon(width, jobs, columnKeys, noTruncate, cordonedTargets{})
}

func newJobListLayoutWithCordon(width int, jobs []*db.Job, columnKeys []string, noTruncate bool, cordoned cordonedTargets) jobListLayout {
	if width <= 0 {
		width = 120
	}

	if len(columnKeys) == 0 {
		columnKeys = responsiveColumnKeys(width)
	}

	projectWidth := computeProjectWidth(jobs)
	defs := columnDefMap()
	columns := make([]columnDef, 0, len(columnKeys))
	hasDescription := false

	for _, key := range columnKeys {
		cd, ok := defs[key]
		if !ok {
			continue
		}
		if key == "project" && cd.width == 0 {
			cd.width = projectWidth
		}
		if key == "description" {
			hasDescription = true
			continue
		}
		if key == "host" {
			cd = decorateHostColumnForCordon(cd, cordoned)
		}
		columns = append(columns, cd)
	}

	if hasDescription {
		descWidth := computeDescriptionWidth(width, columns, noTruncate, jobs)
		descDef := defs["description"]
		descDef.width = descWidth
		columns = append(columns, descDef)
	}

	return jobListLayout{width: width, columns: columns, cordoned: cordoned}
}

func decorateHostColumnForCordon(cd columnDef, cordoned cordonedTargets) columnDef {
	if len(cordoned.launches) == 0 && len(cordoned.hosts) == 0 && len(cordoned.overloadedHosts) == 0 {
		return cd
	}
	inner := cd.value
	cd.value = func(job *db.Job) string {
		s := inner(job)
		if cordoned.IsOverloaded(job) {
			s += " " + overloadMarker
		}
		if cordoned.IsCordoned(job) {
			s += " " + cordonMarker
		}
		return s
	}
	return cd
}

// computeProjectWidth calculates the project column width from actual data.
func computeProjectWidth(jobs []*db.Job) int {
	w := len("PROJECT")
	for _, job := range jobs {
		if pw := len(campaign.JobProjectLabel(job)); pw > w {
			w = pw
		}
	}
	if w > 30 {
		w = 30
	}
	return w
}

// computeDescriptionWidth calculates the width for the description column.
func computeDescriptionWidth(width int, columns []columnDef, noTruncate bool, jobs []*db.Job) int {
	if noTruncate {
		maxDesc := len("DESCRIPTION")
		for _, job := range jobs {
			if w := lipgloss.Width(job.EffectiveDescription()); w > maxDesc {
				maxDesc = w
			}
		}
		return maxDesc
	}

	fixedWidth := 0
	for _, column := range columns {
		fixedWidth += column.width
	}
	separatorWidth := max(0, len(columns))
	descWidth := width - fixedWidth - separatorWidth
	if descWidth < 12 {
		descWidth = 12
	}
	return descWidth
}

func formatJobListHeader(layout jobListLayout) string {
	parts := make([]string, 0, len(layout.columns))
	for _, column := range layout.columns {
		parts = append(parts, padOrTruncateDisplay(column.title, column.width, column.alignRight))
	}
	return strings.Join(parts, " ")
}

func formatJobListRow(layout jobListLayout, job *db.Job) string {
	parts := make([]string, 0, len(layout.columns))
	for _, column := range layout.columns {
		parts = append(parts, padOrTruncateDisplay(column.value(job), column.width, column.alignRight))
	}
	row := strings.Join(parts, " ")
	if layout.cordoned.IsOverloaded(job) {
		row = tuiFailedStyle.Render(row)
	}
	if job != nil && job.DisplayMoveDim {
		row = moveAttemptDimStyle.Render(row)
	}
	return row
}

func formatJobListHost(job *db.Job) string {
	if job == nil {
		return ""
	}
	if strings.TrimSpace(job.DisplayMoveSource) != "" && strings.TrimSpace(job.DisplayMoveTarget) != "" {
		if job.DisplayMoveDim {
			return strings.TrimSpace(job.DisplayMoveTarget)
		}
		return strings.TrimSpace(job.DisplayMoveSource) + " -> " + strings.TrimSpace(job.DisplayMoveTarget)
	}
	if job.TargetKind() == db.JobTargetRentalInstance && job.LaunchID != nil {
		return formatRentalInstanceLabel(job)
	}
	return job.TargetDisplay()
}

func formatJobListTime(job *db.Job) string {
	t := jobListDisplayTime(job)
	if t <= 0 {
		return "-"
	}
	return time.Unix(t, 0).Format("01/02 15:04")
}

func jobListDisplayTime(job *db.Job) int64 {
	if job == nil {
		return 0
	}
	if db.IsTerminalStatus(job.EffectiveStatus()) && job.EndTime != nil && *job.EndTime > 0 {
		return *job.EndTime
	}
	return job.StartTime
}

func formatJobListStatus(job *db.Job) string {
	if job == nil {
		return ""
	}

	status := job.EffectiveStatus()
	if status == db.StatusPendingPlacement {
		if job.TargetKind() == db.JobTargetUnplaced {
			status = "unplaced"
		} else {
			status = "launching"
		}
	}
	if display := queueblock.Display(job, nil); display.Kind != "" {
		status = display.Kind
	}
	if job.DisplayMoveDim {
		status += " non-auth"
	} else if strings.TrimSpace(job.DisplayMoveSource) != "" && strings.TrimSpace(job.DisplayMoveTarget) != "" {
		status += " moving"
	}
	if status == db.StatusCompleted && job.ExitCode != nil {
		if *job.ExitCode == 0 {
			return "completed ok"
		}
		status = fmt.Sprintf("failed (%d)", *job.ExitCode)
		if job.RetryCount > 0 {
			status += " retried"
		} else if job.ErrorDiagnosis != "" {
			status += " diagnosed"
		}
	}
	return status
}

func formatJobListProject(job *db.Job) string {
	if job == nil {
		return ""
	}
	return campaign.JobProjectLabel(job)
}

func padOrTruncateDisplay(value string, width int, alignRight bool) string {
	if width <= 0 {
		return ""
	}
	value = truncateDisplayWidth(value, width)
	padding := width - lipgloss.Width(value)
	if padding < 0 {
		padding = 0
	}
	if alignRight {
		return strings.Repeat(" ", padding) + value
	}
	return value + strings.Repeat(" ", padding)
}

func truncateDisplayWidth(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	return ansi.Truncate(value, width, "…")
}
