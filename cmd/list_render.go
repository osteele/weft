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
	"github.com/osteele/weft/internal/db"
)

type jobListColumn struct {
	title      string
	width      int
	alignRight bool
	value      func(*db.Job) string
}

type jobListLayout struct {
	width   int
	columns []jobListColumn
}

func renderJobListPlain(jobs []*db.Job, width int) string {
	if len(jobs) == 0 {
		return "No jobs found\n"
	}

	layout := newJobListLayout(width)
	lines := make([]string, 0, len(jobs)+1)
	lines = append(lines, truncateDisplayWidth(formatJobListHeader(layout), layout.width))
	for _, job := range jobs {
		lines = append(lines, truncateDisplayWidth(formatJobListRow(layout, job), layout.width))
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

func newJobListLayout(width int) jobListLayout {
	if width <= 0 {
		width = 120
	}

	columns := []jobListColumn{
		{title: "ID", width: 6, alignRight: true, value: func(job *db.Job) string { return fmt.Sprintf("%d", job.ID) }},
	}

	switch {
	case width >= 96:
		columns = append(columns,
			jobListColumn{title: "HOST", width: 12, value: func(job *db.Job) string { return formatJobListHost(job) }},
			jobListColumn{title: "STATUS", width: 14, value: func(job *db.Job) string { return formatJobListStatus(job) }},
			jobListColumn{title: "STARTED", width: 11, value: func(job *db.Job) string { return formatJobListStarted(job) }},
			jobListColumn{title: "DIR", width: 14, value: func(job *db.Job) string { return job.DirectoryTailDisplay() }},
		)
	case width >= 80:
		columns = append(columns,
			jobListColumn{title: "HOST", width: 12, value: func(job *db.Job) string { return formatJobListHost(job) }},
			jobListColumn{title: "STATUS", width: 14, value: func(job *db.Job) string { return formatJobListStatus(job) }},
			jobListColumn{title: "STARTED", width: 11, value: func(job *db.Job) string { return formatJobListStarted(job) }},
		)
	case width >= 64:
		columns = append(columns,
			jobListColumn{title: "HOST", width: 12, value: func(job *db.Job) string { return formatJobListHost(job) }},
			jobListColumn{title: "STATUS", width: 14, value: func(job *db.Job) string { return formatJobListStatus(job) }},
		)
	default:
		columns = append(columns,
			jobListColumn{title: "STATUS", width: 14, value: func(job *db.Job) string { return formatJobListStatus(job) }},
		)
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
	columns = append(columns, jobListColumn{
		title: "DESCRIPTION",
		width: descWidth,
		value: func(job *db.Job) string {
			return job.EffectiveDescription()
		},
	})

	return jobListLayout{width: width, columns: columns}
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
	if strings.TrimSpace(job.Host) == "" {
		return "(unplaced)"
	}
	return job.Host
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
