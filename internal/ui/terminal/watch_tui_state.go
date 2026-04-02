package terminal

import (
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
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

func (m watchModel) computeProjectLines() []string {
	rendered := strings.TrimRight(renderProjectWatchPlain(m.projectGroups, max(20, m.width-2), time.Now(), m.projectRecent), "\n")
	if rendered == "" {
		return nil
	}
	return strings.Split(rendered, "\n")
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
