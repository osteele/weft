package slack

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
)

// SendCampaignNotification sends a Slack message summarizing a completed campaign.
// Errors are logged but not returned — notification failure should not affect the campaign lifecycle.
func SendCampaignNotification(database *sql.DB, campaign db.Campaign) {
	instances, err := db.GetCampaignInstances(database, campaign.ID)
	if err != nil {
		log.Printf("slack: get campaign %d instances: %v", campaign.ID, err)
		return
	}

	// Collect job logs for LLM summarization
	jobLogs := collectJobLogs(database, instances)

	msg := FormatCampaignMessage(campaign, instances)
	if summary := summarizeJobLogs(jobLogs); summary != "" {
		msg += "\n\n" + summary
	}
	if err := Post(msg); err != nil {
		log.Printf("slack: send campaign %d notification: %v", campaign.ID, err)
	}
}

// FormatCampaignMessage builds the Slack notification text for a completed campaign.
func FormatCampaignMessage(campaign db.Campaign, instances []*db.CloudInstance) string {
	var succeeded, failed, cancelled int
	for _, inst := range instances {
		switch inst.Status {
		case db.CloudInstanceStatusCompleted:
			succeeded++
		case db.CloudInstanceStatusFailed:
			failed++
		case db.CloudInstanceStatusCancelled:
			cancelled++
		}
	}

	elapsed := campaignDuration(campaign)

	var b strings.Builder
	fmt.Fprintf(&b, "Campaign %d %s", campaign.ID, campaign.Status)

	var counts []string
	if succeeded > 0 {
		counts = append(counts, fmt.Sprintf("%d succeeded", succeeded))
	}
	if failed > 0 {
		counts = append(counts, fmt.Sprintf("%d failed", failed))
	}
	if cancelled > 0 {
		counts = append(counts, fmt.Sprintf("%d cancelled", cancelled))
	}
	if len(counts) > 0 {
		fmt.Fprintf(&b, " (%s", strings.Join(counts, ", "))
		if elapsed != "" {
			fmt.Fprintf(&b, ", %s elapsed", elapsed)
		}
		b.WriteString(")")
	} else if elapsed != "" {
		fmt.Fprintf(&b, " (%s elapsed)", elapsed)
	}

	for _, inst := range instances {
		icon := "✓"
		if inst.Status == db.CloudInstanceStatusFailed {
			icon = "✗"
		} else if inst.Status == db.CloudInstanceStatusCancelled {
			icon = "–"
		}

		gpu := inst.DisplayGPUSpec()
		if gpu == "" {
			gpu = "unknown GPU"
		}

		dur := instanceDuration(inst)
		durStr := ""
		if dur != "" {
			durStr = " — " + dur
		}

		fmt.Fprintf(&b, "\n  %s instance %d on %s%s", icon, inst.ID, gpu, durStr)
	}

	return b.String()
}

// collectJobLogs gathers cached log content for all jobs across the campaign's instances.
func collectJobLogs(database *sql.DB, instances []*db.CloudInstance) map[int64]string {
	logs := make(map[int64]string)
	for _, inst := range instances {
		jobs, err := db.GetCloudInstanceJobsIncludingAttempts(database, inst.ID)
		if err != nil {
			continue
		}
		for _, job := range jobs {
			if _, exists := logs[job.ID]; exists {
				continue
			}
			content, err := logcache.Read(job.ID)
			if err != nil || content == "" {
				continue
			}
			logs[job.ID] = content
		}
	}
	return logs
}

func campaignDuration(c db.Campaign) string {
	if c.EndedAt == nil {
		return ""
	}
	d := time.Duration(*c.EndedAt-c.CreatedAt) * time.Second
	return formatDuration(d)
}

func instanceDuration(inst *db.CloudInstance) string {
	if inst.EndedAt == nil {
		return ""
	}
	start := inst.CreatedAt
	if inst.LaunchedAt != nil {
		start = *inst.LaunchedAt
	}
	d := time.Duration(*inst.EndedAt-start) * time.Second
	return formatDuration(d)
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
