package cmd

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/progress"
)

func jobProgressSummary(database *sql.DB, job *db.Job) string {
	if job == nil {
		return ""
	}
	if database != nil && job.LaunchID != nil && *job.LaunchID > 0 {
		if live, err := db.GetLaunchLiveState(database, *job.LaunchID); err == nil && live != nil && live.JobProgressPct >= 0 {
			if job.LatestRunID == nil || live.JobProgressID == 0 || live.JobProgressID == *job.LatestRunID {
				if live.JobProgressPhase > 1 {
					return fmt.Sprintf("phase %d, %d%%", live.JobProgressPhase, live.JobProgressPct)
				}
				return fmt.Sprintf("%d%%", live.JobProgressPct)
			}
		}
	}
	content, err := logcache.Read(job.ID)
	if err != nil {
		return ""
	}
	p := progress.FindLastProgressPreferExplicit(content)
	if p == nil {
		return ""
	}
	pct := p.DisplayPercent()
	switch {
	case p.Current > 0 && p.Total > 0 && pct >= 0:
		return appendProgressLabel(fmt.Sprintf("%d/%d (%d%%)", p.Current, p.Total, pct), p)
	case pct >= 0:
		return appendProgressLabel(fmt.Sprintf("%d%%", pct), p)
	default:
		return p.RawLine
	}
}

func appendProgressLabel(summary string, p *progress.Progress) string {
	raw := strings.TrimSpace(p.RawLine)
	if raw == "" || strings.EqualFold(raw, "Progress: "+summary) {
		return summary
	}
	if label := progressLabel(p); label != "" {
		return summary + " " + label
	}
	return summary
}

func progressLabel(p *progress.Progress) string {
	raw := p.RawLine
	lower := strings.ToLower(raw)
	idx := strings.Index(lower, "progress:")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(raw[idx+len("progress:"):])
	fields := strings.Fields(rest)
	switch {
	case p.Percent >= 0:
		if len(fields) <= 1 {
			return ""
		}
		return strings.Join(fields[1:], " ")
	case p.Total > 0 && len(fields) > 3 && strings.EqualFold(fields[1], "of"):
		return strings.Join(fields[3:], " ")
	case p.Total > 0 && len(fields) > 4 && strings.EqualFold(fields[1], "out") && strings.EqualFold(fields[2], "of"):
		return strings.Join(fields[4:], " ")
	case p.Total > 0 && len(fields) > 1:
		return strings.Join(fields[1:], " ")
	default:
		return ""
	}
}
