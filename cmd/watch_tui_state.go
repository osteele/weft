package cmd

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
		count := 0
		for _, id := range m.instanceIDs {
			if !m.cachedHiddenIDs[id] {
				count++
			}
		}
		count += len(m.unplacedJobs)
		return count
	case m.mode == watchModeSystem:
		count := len(m.unplacedJobs)
		for _, ci := range m.cloudInstances {
			if !m.cachedHiddenIDs[ci.ID] {
				count++
			}
		}
		for _, host := range m.onPremHosts {
			count += len(host.Jobs)
		}
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

func (m watchModel) selectedOnPremJob() *db.Job {
	if m.mode != watchModeSystem {
		return nil
	}
	index := m.cursor - m.visibleCloudInstanceCount()
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
		index := m.cursor - m.visibleCloudInstanceCount()
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
		// In campaign/instance mode, selectable rows are: instances, then unplaced jobs
		visibleInstances := 0
		for _, id := range m.instanceIDs {
			if !m.cachedHiddenIDs[id] {
				visibleInstances++
			}
		}
		index := m.cursor - visibleInstances
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
