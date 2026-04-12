package orchestration

import "fmt"

func SummarizeAutoLaunchReasons(reasons map[int64]string, fallback string) string {
	if len(reasons) == 0 {
		return fallback
	}
	counts := map[string]int{}
	for _, reason := range reasons {
		if reason == "" {
			continue
		}
		counts[reason]++
	}
	bestReason := ""
	bestCount := 0
	for reason, count := range counts {
		if count > bestCount {
			bestReason = reason
			bestCount = count
		}
	}
	if bestReason == "" {
		return fallback
	}
	if len(reasons) == 1 || bestCount == len(reasons) {
		return bestReason
	}
	return fmt.Sprintf("%s (+%d similar)", bestReason, len(reasons)-bestCount)
}
