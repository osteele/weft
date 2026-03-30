package cmd

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
	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

type jobListLayout struct {
	width   int
	columns []columnDef
}

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
		return []string{"check", "id", "host", "status", "started", "project", "dir", "description"}
	case width >= 80:
		return []string{"check", "id", "host", "status", "started", "project", "description"}
	case width >= 64:
		return []string{"check", "id", "host", "status", "project", "description"}
	default:
		return []string{"check", "id", "project", "status", "description"}
	}
}

func newJobListLayout(width int, jobs []*db.Job, columnKeys []string, noTruncate bool) jobListLayout {
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
		columns = append(columns, cd)
	}

	if hasDescription {
		descWidth := computeDescriptionWidth(width, columns, noTruncate, jobs)
		descDef := defs["description"]
		descDef.width = descWidth
		columns = append(columns, descDef)
	}

	return jobListLayout{width: width, columns: columns}
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
	return strings.Join(parts, " ")
}

func formatJobListHost(job *db.Job) string {
	if job == nil {
		return ""
	}
	return job.TargetDisplay()
}

func formatJobListStarted(job *db.Job) string {
	if job == nil || job.StartTime <= 0 {
		return "-"
	}
	return time.Unix(job.StartTime, 0).Format("01/02 15:04")
}

func formatJobListStatus(job *db.Job) string {
	if job == nil {
		return ""
	}

	status := job.EffectiveStatus()
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
	if width == 1 {
		return "…"
	}

	var b strings.Builder
	for _, r := range value {
		next := b.String() + string(r)
		if lipgloss.Width(next)+1 > width {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + "…"
}
