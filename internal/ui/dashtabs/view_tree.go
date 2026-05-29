package dashtabs

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

type treeView struct{}

func newTreeView() *treeView                         { return &treeView{} }
func (v *treeView) Title() string                    { return "Tree" }
func (v *treeView) ShortKey() string                 { return "5" }
func (v *treeView) Init() tea.Cmd                    { return nil }
func (v *treeView) Update(_ tea.Msg) (View, tea.Cmd) { return v, nil }

var expRegex = regexp.MustCompile(`EXP-\d+`)

// noExpKey is the bucket label for jobs in a project that have no EXP id.
const noExpKey = "(no EXP)"

func (v *treeView) Render(width, height int, snap Snapshot, _ bool) string {
	if width < 30 {
		return dimStyle.Render("(window too narrow for tree)")
	}
	projects := groupByProjectThenExp(snap.Jobs)
	if len(projects) == 0 {
		return dimStyle.Render("Tree — no jobs to group.")
	}

	projectKeys := make([]string, 0, len(projects))
	for k := range projects {
		projectKeys = append(projectKeys, k)
	}
	sort.Strings(projectKeys)

	var b strings.Builder
	totalExps := 0
	for _, p := range projectKeys {
		totalExps += len(projects[p])
	}
	b.WriteString(titleStyle.Render(fmt.Sprintf("Tree — %d projects, %d experiments", len(projectKeys), totalExps)))
	b.WriteString("  ")
	// Legend so '✓' is read as "completed" rather than a checkbox.
	b.WriteString(dimStyle.Render("legend: "))
	b.WriteString(runningStyle.Render("Nr") + dimStyle.Render("=running "))
	b.WriteString(queuedStyle.Render("Nq") + dimStyle.Render("=queued "))
	b.WriteString(completedStyle.Render("N✓") + dimStyle.Render("=done "))
	b.WriteString(failedStyle.Render("N✗") + dimStyle.Render("=failed"))
	b.WriteString("\n\n")

	// Column widths: experiment label + counts + description.
	// "EXP-XXX  Nr·Nq·N✓·N✗  " plus description; we let description take the rest.
	const expLabelW = 8                      // "EXP-NNN "
	const countsW = 18                       // "0r·0q·0✓·0✗ "
	descW := width - expLabelW - countsW - 4 // 4 = indent + spacing
	if descW < 20 {
		descW = 20
	}

	for _, projName := range projectKeys {
		exps := projects[projName]
		expKeys := make([]string, 0, len(exps))
		for k := range exps {
			expKeys = append(expKeys, k)
		}
		sort.Slice(expKeys, func(i, j int) bool {
			a, c := expKeys[i], expKeys[j]
			// (no EXP) sorts last.
			if a == noExpKey {
				return false
			}
			if c == noExpKey {
				return true
			}
			return a < c
		})

		// Project header row: project name + aggregated counts.
		var agg StatusCounts
		for _, js := range exps {
			c := deriveCounts(js)
			agg.Running += c.Running
			agg.Queued += c.Queued
			agg.Completed += c.Completed
			agg.Failed += c.Failed
			agg.Killed += c.Killed
			agg.Other += c.Other
		}
		b.WriteString(projectHeaderLine(projName, agg))
		b.WriteString("\n")

		for _, expKey := range expKeys {
			jobs := exps[expKey]
			counts := deriveCounts(jobs)
			desc := sampleDescription(jobs, expKey)
			b.WriteString(expLine(expKey, counts, desc, expLabelW, countsW, descW))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	_ = height
	return b.String()
}

func groupByProjectThenExp(jobs []*db.Job) map[string]map[string][]*db.Job {
	out := map[string]map[string][]*db.Job{}
	for _, j := range jobs {
		if j == nil {
			continue
		}
		proj := j.Project
		if proj == "" {
			proj = "(no project)"
		}
		exp := extractExpKey(j)
		if _, ok := out[proj]; !ok {
			out[proj] = map[string][]*db.Job{}
		}
		out[proj][exp] = append(out[proj][exp], j)
	}
	return out
}

// extractExpKey returns the EXP id, or noExpKey if none is parseable from the
// job's description/command. Unlike the original implementation, project name
// is NOT used as a fallback EXP id — projects are now the outer grouping.
func extractExpKey(j *db.Job) string {
	if j == nil {
		return noExpKey
	}
	for _, src := range []string{j.Description, j.GeneratedDescription, j.Command} {
		if m := expRegex.FindString(src); m != "" {
			return m
		}
	}
	return noExpKey
}

func projectHeaderLine(name string, c StatusCounts) string {
	header := lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Render("▾ " + name)
	parts := []string{}
	if c.Running > 0 {
		parts = append(parts, runningStyle.Render(fmt.Sprintf("%dr", c.Running)))
	}
	if c.Queued > 0 {
		parts = append(parts, queuedStyle.Render(fmt.Sprintf("%dq", c.Queued)))
	}
	if c.Completed > 0 {
		parts = append(parts, completedStyle.Render(fmt.Sprintf("%d✓", c.Completed)))
	}
	if c.Failed > 0 {
		parts = append(parts, failedStyle.Render(fmt.Sprintf("%d✗", c.Failed)))
	}
	suffix := ""
	if len(parts) > 0 {
		suffix = "  " + strings.Join(parts, " · ")
	}
	return header + dimStyle.Render(suffix)
}

func expLine(expKey string, c StatusCounts, desc string, expLabelW, countsW, descW int) string {
	expCell := padRight(expKey, expLabelW)
	if expKey == noExpKey {
		expCell = dimStyle.Render(expCell)
	} else {
		expCell = accentStyle.Render(expCell)
	}
	counts := strings.TrimRight(
		fmt.Sprintf("%s · %s · %s%s",
			runningStyle.Render(fmt.Sprintf("%dr", c.Running)),
			queuedStyle.Render(fmt.Sprintf("%dq", c.Queued)),
			completedStyle.Render(fmt.Sprintf("%d✓", c.Completed)),
			func() string {
				if c.Failed > 0 {
					return " · " + failedStyle.Render(fmt.Sprintf("%d✗", c.Failed))
				}
				return ""
			}(),
		), " ")
	countsCell := lipgloss.NewStyle().Width(countsW).Render(counts)
	descCell := shortStr(desc, descW)
	return fmt.Sprintf("    %s %s %s", expCell, countsCell, dimStyle.Render(descCell))
}

func sampleDescription(jobs []*db.Job, expKey string) string {
	for _, j := range jobs {
		d := j.Description
		if d == "" {
			d = j.GeneratedDescription
		}
		if d == "" {
			continue
		}
		d = singleLine(d)
		// Strip the EXP-NNN prefix if present so we don't repeat it.
		if expKey != noExpKey {
			d = strings.TrimSpace(strings.TrimPrefix(d, expKey+":"))
			d = strings.TrimSpace(strings.TrimPrefix(d, expKey))
		}
		return d
	}
	return ""
}
