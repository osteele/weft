package cmd

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
)

type projectGroup struct {
	Label       string
	Directories []string
	CloudInsts  []*db.Launch
	Jobs        []*db.Job
	Running     []*db.Job
	Queued      []*db.Job
	Recent      []*db.Job
}

func openJobsDB() (*sql.DB, error) {
	database, err := db.Open()
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return database, nil
}

func groupJobsByProject(jobs []*db.Job) []projectGroup {
	groups := make(map[string]*projectGroup)
	order := make([]string, 0)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		key := projectGroupLabel(job)
		group := groups[key]
		if group == nil {
			group = &projectGroup{Label: key}
			groups[key] = group
			order = append(order, key)
		}
		projectGroupAddDirectory(group, job)
		group.Jobs = append(group.Jobs, job)
	}

	sort.Strings(order)
	result := make([]projectGroup, 0, len(order))
	for _, key := range order {
		group := groups[key]
		sort.Strings(group.Directories)
		result = append(result, *group)
	}
	return result
}

func groupProjectActivity(activeJobs, recentJobs []*db.Job) []projectGroup {
	groups := make(map[string]*projectGroup)
	order := make([]string, 0)
	addJob := func(job *db.Job, bucket string) {
		if job == nil {
			return
		}
		key := projectGroupLabel(job)
		group := groups[key]
		if group == nil {
			group = &projectGroup{Label: key}
			groups[key] = group
			order = append(order, key)
		}
		projectGroupAddDirectory(group, job)
		switch bucket {
		case "running":
			group.Running = append(group.Running, job)
		case "queued":
			group.Queued = append(group.Queued, job)
		case "recent":
			group.Recent = append(group.Recent, job)
		}
	}

	for _, job := range activeJobs {
		addJob(job, projectActivityBucket(job))
	}
	for _, job := range recentJobs {
		addJob(job, "recent")
	}

	sort.Strings(order)
	result := make([]projectGroup, 0, len(order))
	for _, key := range order {
		group := groups[key]
		sort.Strings(group.Directories)
		sort.SliceStable(group.Running, func(i, j int) bool {
			return projectRunningLess(group.Running[i], group.Running[j])
		})
		sort.SliceStable(group.Queued, func(i, j int) bool {
			return projectQueuedLess(group.Queued[i], group.Queued[j])
		})
		sort.SliceStable(group.Recent, func(i, j int) bool {
			return projectRecentLess(group.Recent[i], group.Recent[j])
		})
		result = append(result, *group)
	}
	return result
}

func projectGroupLabel(job *db.Job) string {
	if job == nil {
		return "(no project)"
	}
	if project := strings.TrimSpace(job.Project); project != "" {
		return project
	}
	if dir := strings.TrimSpace(job.DirectoryTailDisplay()); dir != "" && dir != "—" {
		return dir
	}
	return "(no project)"
}

func projectGroupAddDirectory(group *projectGroup, job *db.Job) {
	dir := strings.TrimSpace(job.EffectiveWorkingDir())
	if dir == "" {
		return
	}
	for _, existing := range group.Directories {
		if existing == dir {
			return
		}
	}
	group.Directories = append(group.Directories, dir)
}

func projectActivityBucket(job *db.Job) string {
	switch job.EffectiveStatus() {
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return "running"
	case db.StatusQueued, db.StatusPendingPlacement:
		return "queued"
	default:
		return "queued"
	}
}

func projectRunningLess(a, b *db.Job) bool {
	if a.StartTime != b.StartTime {
		return a.StartTime < b.StartTime
	}
	return a.ID < b.ID
}

func projectQueuedLess(a, b *db.Job) bool {
	aTime := a.QueuedAt
	if aTime == 0 {
		aTime = a.CreatedAt
	}
	bTime := b.QueuedAt
	if bTime == 0 {
		bTime = b.CreatedAt
	}
	if aTime != bTime {
		return aTime < bTime
	}
	return a.ID < b.ID
}

func projectRecentLess(a, b *db.Job) bool {
	var aEnd, bEnd int64
	if a.EndTime != nil {
		aEnd = *a.EndTime
	}
	if b.EndTime != nil {
		bEnd = *b.EndTime
	}
	if aEnd != bEnd {
		return aEnd > bEnd
	}
	return a.ID > b.ID
}

func renderProjectListPlain(groups []projectGroup, width int) string {
	if len(groups) == 0 {
		return "No projects found\n"
	}

	// Compute column widths
	maxNameLen := len("PROJECT")
	for _, g := range groups {
		if len(g.Label) > maxNameLen {
			maxNameLen = len(g.Label)
		}
	}
	if maxNameLen > 30 {
		maxNameLen = 30
	}

	now := time.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %5s  %7s  %6s  %6s  %s\n", maxNameLen, "PROJECT", "JOBS", "RUNNING", "QUEUED", "FAILED", "LAST ACTIVITY")
	for _, g := range groups {
		var running, queued, failed int
		var lastActivity int64
		for _, job := range g.Jobs {
			switch job.EffectiveStatus() {
			case db.StatusRunning, db.StatusStarting, db.StatusPaused:
				running++
			case db.StatusQueued, db.StatusPendingPlacement:
				queued++
			case db.StatusFailed, db.StatusDead:
				failed++
			case db.StatusCompleted:
				if job.ExitCode != nil && *job.ExitCode != 0 {
					failed++
				}
			}
			if job.EndTime != nil && *job.EndTime > lastActivity {
				lastActivity = *job.EndTime
			}
			if job.StartTime > lastActivity {
				lastActivity = job.StartTime
			}
		}

		label := g.Label
		if len(label) > maxNameLen {
			label = label[:maxNameLen-1] + "…"
		}

		var activityStr string
		if lastActivity == 0 {
			activityStr = "-"
		} else {
			activityStr = shortRelativeTime(now.Unix() - lastActivity)
		}

		fmt.Fprintf(&b, "%-*s  %5d  %7d  %6d  %6d  %s\n",
			maxNameLen, label, len(g.Jobs), running, queued, failed, activityStr)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func renderProjectJobsPlain(groups []projectGroup, width int) string {
	if len(groups) == 0 {
		return "No jobs found\n"
	}

	var allJobs []*db.Job
	for _, group := range groups {
		allJobs = append(allJobs, group.Jobs...)
	}
	layout := newJobListLayout(width, allJobs)
	var b strings.Builder
	for i, group := range groups {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(truncateDisplayWidth(group.Label, width))
		b.WriteString("\n")
		for _, dir := range group.Directories {
			b.WriteString(truncateDisplayWidth("  dir: "+dir, width))
			b.WriteString("\n")
		}
		b.WriteString(truncateDisplayWidth(formatJobListHeader(layout), width))
		b.WriteString("\n")
		for _, job := range group.Jobs {
			b.WriteString(truncateDisplayWidth(formatJobListRow(layout, job), width))
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func renderProjectWatchPlain(groups []projectGroup, width int, now time.Time, recentWindow time.Duration) string {
	if len(groups) == 0 {
		return "No project activity\n"
	}

	var b strings.Builder
	for i, group := range groups {
		if i > 0 {
			b.WriteString("\n")
		}
		header := fmt.Sprintf("%s (%d running, %d queued, %d recent/%s)",
			group.Label, len(group.Running), len(group.Queued), len(group.Recent), formatProjectRecentWindow(recentWindow))
		b.WriteString(truncateDisplayWidth(header, width))
		b.WriteString("\n")
		for _, dir := range group.Directories {
			b.WriteString(truncateDisplayWidth("  dir: "+dir, width))
			b.WriteString("\n")
		}
		if len(group.Running) > 0 {
			b.WriteString("  Running\n")
			for _, job := range group.Running {
				b.WriteString(truncateDisplayWidth("    "+formatProjectWatchRow(job, "running", now), width))
				b.WriteString("\n")
			}
		}
		if len(group.Queued) > 0 {
			b.WriteString("  Queued\n")
			for _, job := range group.Queued {
				b.WriteString(truncateDisplayWidth("    "+formatProjectWatchRow(job, "queued", now), width))
				b.WriteString("\n")
			}
		}
		if len(group.CloudInsts) > 0 {
			b.WriteString("  Rental instances\n")
			for _, inst := range group.CloudInsts {
				b.WriteString(truncateDisplayWidth("    "+formatProjectLaunchRow(inst, now), width))
				b.WriteString("\n")
			}
		}
		if len(group.Recent) > 0 {
			b.WriteString("  Recent\n")
			for _, job := range group.Recent {
				b.WriteString(truncateDisplayWidth("    "+formatProjectWatchRow(job, "recent", now), width))
				b.WriteString("\n")
			}
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func formatProjectWatchRow(job *db.Job, bucket string, now time.Time) string {
	status := formatJobListStatus(job)
	host := formatJobListHost(job)
	dir := job.DirectoryTailDisplay()
	when := formatProjectWatchTime(job, bucket, now)
	desc := job.EffectiveDescription()
	if desc == "" {
		desc = job.EffectiveCommand()
	}
	return fmt.Sprintf("#%-5d %-16s %-12s %-14s %-12s %s", job.ID, status, host, dir, when, desc)
}

func formatProjectLaunchRow(inst *db.Launch, now time.Time) string {
	if inst == nil {
		return ""
	}
	status := watchInstanceStatusLabel(inst, nil)
	if label := inst.GraceStatusLabel(); label != "" {
		status = label
	}
	metrics := formatLaunchMetricsInline(observeLaunch(inst, nil, now))
	if metrics == "" {
		return fmt.Sprintf("instance %-5d %-24s %s", inst.ID, inst.DisplayGPUSpec(), status)
	}
	return fmt.Sprintf("instance %-5d %-24s %-18s %s", inst.ID, inst.DisplayGPUSpec(), status, metrics)
}

func formatProjectWatchTime(job *db.Job, bucket string, now time.Time) string {
	var ts int64
	switch bucket {
	case "recent":
		if job.EndTime != nil {
			ts = *job.EndTime
		}
	case "running":
		ts = job.StartTime
	case "queued":
		ts = job.QueuedAt
		if ts == 0 {
			ts = job.CreatedAt
		}
	}
	if ts == 0 {
		return "-"
	}
	return shortRelativeTime(now.Unix() - ts)
}

func shortRelativeTime(deltaSeconds int64) string {
	if deltaSeconds < 0 {
		deltaSeconds = 0
	}
	switch {
	case deltaSeconds < 60:
		return fmt.Sprintf("%ds ago", deltaSeconds)
	case deltaSeconds < 3600:
		return fmt.Sprintf("%dm ago", deltaSeconds/60)
	case deltaSeconds < 86400:
		return fmt.Sprintf("%dh ago", deltaSeconds/3600)
	default:
		return fmt.Sprintf("%dd ago", deltaSeconds/86400)
	}
}

func formatProjectRecentWindow(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.String()
}
