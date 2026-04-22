package terminal

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/queueblock"
)

// ---------------------------------------------------------------------------
// Cursor / selection helpers
// ---------------------------------------------------------------------------

func (m *watchModel) moveCursor(delta int) {
	count := m.selectableRowCount()
	if count == 0 {
		m.cursor = 0
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= count {
		m.cursor = count - 1
	}
}

func (m watchModel) pageSize() int {
	if m.height > 4 {
		return m.height / 2
	}
	return max(1, m.height)
}

func (m *watchModel) clampCursor() {
	count := m.selectableRowCount()
	if count == 0 {
		m.cursor = 0
		return
	}
	if m.cursor >= count {
		m.cursor = count - 1
	}
}

func (m watchModel) selectableRowCount() int {
	switch {
	case m.mode.isInstanceBased():
		count := m.selectableCloudRowCount()
		for _, host := range m.onPremHosts {
			count += len(host.Jobs)
		}
		count += len(m.unplacedJobs)
		return count
	case m.mode == watchModeSystem:
		count := m.selectableCloudRowCount()
		for _, host := range m.onPremHosts {
			count += len(host.Jobs)
		}
		count += len(m.unplacedJobs)
		return count
	case m.mode == watchModeProject:
		return len(m.projectLines)
	}
	return 0
}

func (m watchModel) visibleCloudInstanceCount() int {
	count := 0
	for _, ci := range m.cloudInstances {
		if !m.cachedHiddenIDs[ci.ID] {
			count++
		}
	}
	return count
}

// selectableCloudRowCount returns the total number of selectable rows in the
// cloud instances section: one header per visible instance plus one per job.
func (m watchModel) selectableCloudRowCount() int {
	count := 0
	switch {
	case m.mode == watchModeSystem:
		for _, ci := range m.cloudInstances {
			if m.cachedHiddenIDs[ci.ID] {
				continue
			}
			count++ // instance header
			if u, ok := m.updates[ci.ID]; ok {
				count += len(u.Jobs)
			}
		}
	case m.mode.isInstanceBased():
		for _, id := range m.instanceIDs {
			if m.cachedHiddenIDs[id] {
				continue
			}
			count++ // instance header
			if u, ok := m.updates[id]; ok {
				count += len(u.Jobs)
			} else if info, ok := m.initInfo[id]; ok {
				count += len(info.jobs)
			}
		}
	}
	return count
}

// selectedCloudInstanceID returns the cloud instance ID when the cursor is
// on an instance header row, or 0 if the cursor is elsewhere.
func (m watchModel) selectedCloudInstanceID() int64 {
	index := m.cursor
	switch {
	case m.mode == watchModeSystem:
		for _, ci := range m.cloudInstances {
			if m.cachedHiddenIDs[ci.ID] {
				continue
			}
			if index == 0 {
				return ci.ID
			}
			index--
			if u, ok := m.updates[ci.ID]; ok {
				index -= len(u.Jobs)
			}
			if index < 0 {
				return 0
			}
		}
	case m.mode.isInstanceBased():
		for _, id := range m.instanceIDs {
			if m.cachedHiddenIDs[id] {
				continue
			}
			if index == 0 {
				return id
			}
			index--
			if u, ok := m.updates[id]; ok {
				index -= len(u.Jobs)
			} else if info, ok := m.initInfo[id]; ok {
				index -= len(info.jobs)
			}
			if index < 0 {
				return 0
			}
		}
	}
	return 0
}

// selectedCloudJob returns the cloud instance job at the cursor position,
// or nil if the cursor is not on a cloud job row.
func (m watchModel) selectedCloudJob() *db.Job {
	index := m.cursor
	switch {
	case m.mode == watchModeSystem:
		for _, ci := range m.cloudInstances {
			if m.cachedHiddenIDs[ci.ID] {
				continue
			}
			if index == 0 {
				return nil // cursor on instance header
			}
			index--
			u, ok := m.updates[ci.ID]
			if !ok {
				continue
			}
			if index < len(u.Jobs) {
				return u.Jobs[index]
			}
			index -= len(u.Jobs)
		}
	case m.mode.isInstanceBased():
		for _, id := range m.instanceIDs {
			if m.cachedHiddenIDs[id] {
				continue
			}
			if index == 0 {
				return nil // cursor on instance header
			}
			index--
			var jobs []*db.Job
			if u, ok := m.updates[id]; ok {
				jobs = u.Jobs
			} else if info, ok := m.initInfo[id]; ok {
				jobs = info.jobs
			}
			if index < len(jobs) {
				return jobs[index]
			}
			index -= len(jobs)
		}
	}
	return nil
}

func (m watchModel) selectedOnPremJob() *db.Job {
	if !(m.mode == watchModeSystem || m.mode.isInstanceBased()) {
		return nil
	}
	index := m.cursor - m.selectableCloudRowCount()
	if index < 0 {
		return nil
	}
	for _, host := range m.onPremHosts {
		if index < len(host.Jobs) {
			return host.Jobs[index]
		}
		index -= len(host.Jobs)
	}
	return nil
}

func (m watchModel) selectedUnplacedJob() *db.Job {
	switch m.mode {
	case watchModeSystem:
		index := m.cursor - m.selectableCloudRowCount()
		if index < 0 {
			return nil
		}
		for _, host := range m.onPremHosts {
			if index < len(host.Jobs) {
				return nil
			}
			index -= len(host.Jobs)
		}
		if index < 0 || index >= len(m.unplacedJobs) {
			return nil
		}
		return m.unplacedJobs[index]
	default:
		// In campaign/instance mode, selectable rows are: instance headers + jobs,
		// then on-prem host jobs, then unplaced jobs.
		index := m.cursor - m.selectableCloudRowCount()
		if index < 0 {
			return nil
		}
		for _, host := range m.onPremHosts {
			if index < len(host.Jobs) {
				return nil
			}
			index -= len(host.Jobs)
		}
		if index < 0 || index >= len(m.unplacedJobs) {
			return nil
		}
		return m.unplacedJobs[index]
	}
}

// selectedAnyJob returns whichever job is under the cursor across the three
// job buckets (cloud, on-prem, unplaced). Used by the attempts drill-down,
// which only needs the job ID.
func (m watchModel) selectedAnyJob() *db.Job {
	if job := m.selectedCloudJob(); job != nil {
		return job
	}
	if job := m.selectedOnPremJob(); job != nil {
		return job
	}
	return m.selectedUnplacedJob()
}

func (m *watchModel) removeOnPremJob(jobID int64) {
	filteredHosts := m.onPremHosts[:0]
	for _, host := range m.onPremHosts {
		jobs := host.Jobs[:0]
		for _, job := range host.Jobs {
			if job != nil && job.ID == jobID {
				continue
			}
			jobs = append(jobs, job)
		}
		if len(jobs) == 0 {
			continue
		}
		host.Jobs = jobs
		filteredHosts = append(filteredHosts, host)
	}
	m.onPremHosts = filteredHosts
}

func (m *watchModel) upsertUnplacedJob(job *db.Job) {
	if job == nil {
		return
	}
	for i, existing := range m.unplacedJobs {
		if existing != nil && existing.ID == job.ID {
			m.unplacedJobs[i] = job
			return
		}
	}
	m.unplacedJobs = append(m.unplacedJobs, job)
	sort.SliceStable(m.unplacedJobs, func(i, j int) bool {
		return m.unplacedJobs[i].ID < m.unplacedJobs[j].ID
	})
}

func (m *watchModel) removeUnplacedJob(jobID int64) {
	filtered := m.unplacedJobs[:0]
	for _, job := range m.unplacedJobs {
		if job != nil && job.ID != jobID {
			filtered = append(filtered, job)
		}
	}
	m.unplacedJobs = filtered
}

// ---------------------------------------------------------------------------
// Replacement chain cache
// ---------------------------------------------------------------------------

func (m *watchModel) rebuildReplacementCache() {
	hiddenIDs := make(map[int64]bool)
	replacementChains := make(map[int64][]*db.Launch)

	getCI := func(id int64) *db.Launch {
		if u, ok := m.updates[id]; ok && u.Launch != nil {
			return u.Launch
		}
		if m.initInfo != nil {
			if info, ok := m.initInfo[id]; ok && info.ci != nil {
				return info.ci
			}
		}
		ci, _ := db.GetLaunch(m.database, id)
		return ci
	}

	for _, id := range m.instanceIDs {
		ci := getCI(id)
		if ci == nil || ci.ReplacedInstanceID == nil {
			continue
		}
		chain := collectReplacementChain(ci, getCI)
		if len(chain) > 0 {
			replacementChains[id] = chain
			for _, predecessor := range chain {
				hiddenIDs[predecessor.ID] = true
			}
		}
	}

	m.cachedHiddenIDs = hiddenIDs
	m.cachedReplacementChains = replacementChains
}

// ---------------------------------------------------------------------------
// Project-mode helpers
// ---------------------------------------------------------------------------

func (m watchModel) computeProjectLines() ([]string, []projectLineMeta) {
	width := max(20, m.width-2)
	now := time.Now()

	var lines []string
	var meta []projectLineMeta
	addLine := func(text string, lineMeta projectLineMeta) {
		lines = append(lines, truncateDisplayWidth(text, width))
		meta = append(meta, lineMeta)
	}

	for groupIndex, group := range m.projectGroups {
		if groupIndex > 0 {
			addLine("", projectLineMeta{})
		}

		headerParts := make([]string, 0, 4)
		if len(group.Running) > 0 {
			headerParts = append(headerParts, fmt.Sprintf("%d running", len(group.Running)))
		}
		if len(group.Queued) > 0 {
			headerParts = append(headerParts, fmt.Sprintf("%d queued", len(group.Queued)))
		}
		if len(group.Unplaced) > 0 {
			headerParts = append(headerParts, fmt.Sprintf("%d unplaced", len(group.Unplaced)))
		}
		headerParts = append(headerParts, fmt.Sprintf("%d recent/%s", len(group.Recent), formatProjectRecentWindow(m.projectRecent)))
		header := fmt.Sprintf("%s (%s)", group.Label, strings.Join(headerParts, ", "))
		addLine(header, projectLineMeta{})

		for _, dir := range group.Directories {
			addLine("  dir: "+dir, projectLineMeta{})
		}
		if len(group.Running) > 0 {
			addLine("  Running", projectLineMeta{})
			for _, job := range group.Running {
				addLine("    "+formatProjectWatchRow(job, "running", now), projectLineMeta{job: job, bucket: "running"})
			}
		}
		if len(group.Queued) > 0 {
			addLine("  Queued", projectLineMeta{})
			for _, job := range group.Queued {
				addLine("    "+formatProjectWatchRow(job, "queued", now), projectLineMeta{job: job, bucket: "queued"})
			}
		}
		if len(group.Unplaced) > 0 {
			addLine("  Unplaced", projectLineMeta{})
			for _, job := range group.Unplaced {
				row := formatProjectWatchRow(job, "queued", now) + "  " + formatWatchGPUConstraint(job)
				addLine("    "+row, projectLineMeta{job: job, bucket: "unplaced"})
			}
		}
		if len(group.CloudInsts) > 0 {
			addLine("  Rental instances", projectLineMeta{})
			for _, inst := range group.CloudInsts {
				addLine("    "+formatProjectLaunchRow(inst, now), projectLineMeta{})
			}
		}
		if len(group.Recent) > 0 {
			addLine("  Recent", projectLineMeta{})
			for _, job := range group.Recent {
				addLine("    "+formatProjectWatchRow(job, "recent", now), projectLineMeta{job: job, bucket: "recent"})
			}
		}
	}

	return lines, meta
}

func (m watchModel) selectedProjectStatusDetail() string {
	if m.mode != watchModeProject {
		return ""
	}
	if m.cursor < 0 || m.cursor >= len(m.projectMeta) {
		return ""
	}
	lineMeta := m.projectMeta[m.cursor]
	if lineMeta.job == nil || lineMeta.bucket != "queued" {
		return ""
	}
	display := queueblock.Display(lineMeta.job, nil)
	if !display.Blocked {
		return ""
	}
	reason := strings.TrimSpace(display.Reason)
	if reason == "" {
		return ""
	}
	return fmt.Sprintf("#%d blocked: %s", lineMeta.job.ID, reason)
}

func (m watchModel) selectedProjectJob() (*db.Job, string) {
	if m.mode != watchModeProject {
		return nil, ""
	}
	if m.cursor < 0 || m.cursor >= len(m.projectMeta) {
		return nil, ""
	}
	lineMeta := m.projectMeta[m.cursor]
	return lineMeta.job, lineMeta.bucket
}

// adjustProjectOffset keeps projectOffset in sync with the cursor so the
// cursor is always visible within the page.
func (m *watchModel) adjustProjectOffset() {
	pageSize := m.projectPageSize()
	if pageSize <= 0 {
		m.projectOffset = 0
		return
	}
	if m.cursor < m.projectOffset {
		m.projectOffset = m.cursor
	}
	if m.cursor >= m.projectOffset+pageSize {
		m.projectOffset = m.cursor - pageSize + 1
	}
	maxOffset := max(0, len(m.projectLines)-pageSize)
	if m.projectOffset > maxOffset {
		m.projectOffset = maxOffset
	}
	if m.projectOffset < 0 {
		m.projectOffset = 0
	}
}

func (m watchModel) projectPageSize() int {
	if m.height <= 0 {
		return 10
	}
	return max(1, m.height-3) // title + footer + padding
}
