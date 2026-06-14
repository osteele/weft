package orchestration

import (
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
)

var (
	runRateHeadroomExhaustedRE = regexp.MustCompile(`^run-rate headroom exhausted \([^)]*this group needs \$([0-9]+(?:\.[0-9]{1,2})?)/hr\)$`)
	runRateNoSubsetRE          = regexp.MustCompile(`^run-rate target exceeded .*cheapest group \$([0-9]+(?:\.[0-9]{1,2})?)/hr\)$`)
)

func RunRateHeadroom(database *sql.DB, targetCents int) (int, bool) {
	if database == nil || targetCents <= 0 {
		return 0, false
	}
	current, err := db.SumActiveLaunchCostPerHourCents(database)
	if err != nil {
		return 0, false
	}
	if current >= targetCents {
		return 0, true
	}
	return targetCents - current, true
}

func IsStaleRunRateBlockReason(reason string, headroomCents int) bool {
	pruned, changed := PruneStaleRunRateBlockReason(reason, headroomCents)
	return changed && strings.TrimSpace(pruned) == ""
}

func PruneStaleRunRateBlockReason(reason string, headroomCents int) (string, bool) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", false
	}
	if blockreason.IsReuseOnlyDiagnostic(reason) {
		return "", true
	}
	parts := strings.Split(reason, "; ")
	kept := make([]string, 0, len(parts))
	changed := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		needed, ok := ParseRunRateBlockedNeedCents(part)
		if ok && needed > 0 && headroomCents >= needed {
			changed = true
			continue
		}
		kept = append(kept, part)
	}
	if !changed {
		return reason, false
	}
	pruned := strings.Join(kept, "; ")
	if blockreason.IsReuseOnlyDiagnostic(pruned) {
		return "", true
	}
	return pruned, true
}

func ParseRunRateBlockedNeedCents(reason string) (int, bool) {
	reason = strings.TrimSpace(reason)
	for _, re := range []*regexp.Regexp{runRateHeadroomExhaustedRE, runRateNoSubsetRE} {
		matches := re.FindStringSubmatch(reason)
		if len(matches) != 2 {
			continue
		}
		value, err := strconv.ParseFloat(matches[1], 64)
		if err != nil {
			return 0, false
		}
		return int(value*100 + 0.5), true
	}
	return 0, false
}

func AutoPilotBlockSummary(reasons map[int64]string) string {
	if len(reasons) == 0 {
		return ""
	}
	unique := map[string]int{}
	for _, reason := range reasons {
		r := strings.TrimSpace(reason)
		if r != "" {
			unique[r]++
		}
	}
	if len(unique) == 0 {
		return ""
	}
	if len(unique) == 1 {
		for reason, count := range unique {
			if count == 1 {
				return reason
			}
			return fmt.Sprintf("%s (%d jobs)", reason, count)
		}
	}
	total := 0
	for _, c := range unique {
		total += c
	}
	return fmt.Sprintf("%d jobs blocked", total)
}

func CheckRunRateProjection(database *sql.DB, targetCents, plannedCents int) (string, bool) {
	if database == nil || targetCents <= 0 || plannedCents <= 0 {
		return "", false
	}
	currentRateCents, err := db.SumActiveLaunchCostPerHourCents(database)
	if err != nil {
		return "", false
	}
	projectedRate := currentRateCents + plannedCents
	if projectedRate <= targetCents {
		return "", false
	}
	return fmt.Sprintf(
		"auto-launch: run-rate target exceeded: target %s, current %s + planned %s = %s",
		formatRunRateCents(targetCents),
		formatRunRateCents(currentRateCents),
		formatRunRateCents(plannedCents),
		formatRunRateCents(projectedRate),
	), true
}

func formatRunRateCents(cents int) string {
	if cents <= 0 {
		return "off"
	}
	return fmt.Sprintf("$%.2f/hr", float64(cents)/100)
}
