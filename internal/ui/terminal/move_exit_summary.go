package terminal

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// FormatMoveExitSummary prints the receipt shown after the move launch TUI exits.
func FormatMoveExitSummary(database *sql.DB, jobs []*db.Job, instanceIDs []int64) string {
	return FormatMoveExitSummaryAt(database, jobs, instanceIDs, time.Now(), listOutputWidth())
}

func FormatMoveExitSummaryAt(database *sql.DB, jobs []*db.Job, instanceIDs []int64, now time.Time, width int) string {
	if len(jobs) == 0 && len(instanceIDs) == 0 {
		return ""
	}
	if width <= 0 {
		width = 120
	}

	var b strings.Builder
	writeSummaryLine(&b, width, fmt.Sprintf("weft move ended - %s", now.Format("Mon Jan 2 15:04")))
	if len(jobs) > 0 {
		writeSummaryLine(&b, width, fmt.Sprintf("Moved: %s -> %s", campaign.FormatJobIDs(jobs, 8), pluralize(len(instanceIDs), "new instance", "new instances")))
	}
	if len(instanceIDs) > 0 {
		writeSummaryLine(&b, width, "")
		writeSummaryLine(&b, width, "Instances:")
		for _, id := range instanceIDs {
			writeSummaryLine(&b, width, moveExitInstanceLine(database, id))
		}
	}
	writeSummaryLine(&b, width, "")
	writeSummaryLine(&b, width, "Next: weft watch instance | weft uj | weft log <job>")
	return b.String()
}

func moveExitInstanceLine(database *sql.DB, instanceID int64) string {
	parts := []string{"  " + ids.FormatInstanceID(instanceID)}
	if database != nil {
		if launch, err := db.GetLaunch(database, instanceID); err == nil && launch != nil {
			if gpu := strings.TrimSpace(launch.DisplayGPUBrief()); gpu != "" {
				parts = append(parts, gpu)
			}
			if status := strings.TrimSpace(launch.Status); status != "" {
				parts = append(parts, status)
			}
		}
	}
	return strings.Join(parts, "  ")
}
