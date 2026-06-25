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
	runRateSingleJobRE         = regexp.MustCompile(`job needs \$([0-9]+(?:\.[0-9]{1,2})?)/hr`)
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
	for _, re := range []*regexp.Regexp{runRateHeadroomExhaustedRE, runRateNoSubsetRE, runRateSingleJobRE} {
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

func FormatRunRateTargetExceededReason(target, current, requested, projected, headroom, cheapest int, launchJobIDs []int64) string {
	if len(launchJobIDs) == 1 {
		return fmt.Sprintf(
			"run-rate target exceeded: target %s/hr, current %s/hr + requested %s/hr = %s/hr (headroom %s/hr, job needs %s/hr)",
			formatRateCents(target),
			formatRateCents(current),
			formatRateCents(requested),
			formatRateCents(projected),
			formatRateCents(headroom),
			formatRateCents(cheapest),
		)
	}
	return fmt.Sprintf(
		"run-rate target exceeded (no subset fits): target %s/hr, current %s/hr + requested %s/hr = %s/hr (headroom %s/hr, cheapest group %s/hr)",
		formatRateCents(target),
		formatRateCents(current),
		formatRateCents(requested),
		formatRateCents(projected),
		formatRateCents(headroom),
		formatRateCents(cheapest),
	)
}

func AutoPilotBlockSummary(reasons map[int64]string) string {
	summary, _ := AutoPilotBlockSummaryWithCount(reasons)
	return summary
}

func AutoPilotBlockSummaryWithCount(reasons map[int64]string) (string, int) {
	if len(reasons) == 0 {
		return "", 0
	}
	unique := map[string]int{}
	for _, reason := range reasons {
		r := strings.TrimSpace(reason)
		if r != "" && blockreason.ReasonKind(r) == blockreason.KindBlocked {
			unique[r]++
		}
	}
	if len(unique) == 0 {
		return "", 0
	}
	if len(unique) == 1 {
		for reason, count := range unique {
			if count == 1 {
				return reason, count
			}
			return fmt.Sprintf("%s (%d jobs)", reason, count), count
		}
	}
	total := 0
	for _, c := range unique {
		total += c
	}
	return fmt.Sprintf("%d jobs blocked", total), total
}

func AutoPilotBlockedReasonCount(reasons map[int64]string) int {
	_, count := AutoPilotBlockSummaryWithCount(reasons)
	return count
}

func CheckRunRateProjection(database *sql.DB, targetCents, requestedCents int) (string, bool) {
	if database == nil || targetCents <= 0 || requestedCents <= 0 {
		return "", false
	}
	currentRateCents, err := db.SumActiveLaunchCostPerHourCents(database)
	if err != nil {
		return "", false
	}
	projectedRate := currentRateCents + requestedCents
	if projectedRate <= targetCents {
		return "", false
	}
	return fmt.Sprintf(
		"auto-launch: run-rate target exceeded: target %s, current %s + requested %s = %s",
		formatRunRateCents(targetCents),
		formatRunRateCents(currentRateCents),
		formatRunRateCents(requestedCents),
		formatRunRateCents(projectedRate),
	), true
}

func formatRunRateCents(cents int) string {
	if cents <= 0 {
		return "off"
	}
	return fmt.Sprintf("$%.2f/hr", float64(cents)/100)
}
