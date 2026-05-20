package orchestration

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/ids"
)

var graceAckKeyPattern = regexp.MustCompile(`grace/(\d+)/acks/`)

func SummarizeAutoPilotError(err error) string {
	if err == nil {
		return "unknown error"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled (will retry)"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out (will retry)"
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "unknown error"
	}
	if strings.Contains(msg, "submit jobs to instance control plane:") {
		if matches := graceAckKeyPattern.FindStringSubmatch(msg); len(matches) == 2 {
			if instanceID, err := strconv.ParseInt(matches[1], 10, 64); err == nil {
				return NormalizeStatusLineText(fmt.Sprintf("instance %s did not acknowledge queued jobs; auto-pilot will retry (run `weft sync` to force reconcile)", ids.FormatInstanceID(instanceID)))
			}
			return NormalizeStatusLineText(fmt.Sprintf("instance %s did not acknowledge queued jobs; auto-pilot will retry (run `weft sync` to force reconcile)", matches[1]))
		}
		return NormalizeStatusLineText("instance control plane did not acknowledge queued jobs; auto-pilot will retry (run `weft sync` to force reconcile)")
	}
	return capStatusLine(NormalizeStatusLineText(msg))
}

func NormalizeStatusLineText(msg string) string {
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	msg = strings.ReplaceAll(msg, "\r", "\n")
	parts := strings.Split(msg, "\n")
	for i, part := range parts {
		parts[i] = strings.Join(strings.Fields(part), " ")
	}
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			clean = append(clean, part)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	return strings.Join(clean, " | ")
}

func capStatusLine(msg string) string {
	const maxLen = 140
	if len(msg) <= maxLen {
		return msg
	}
	return strings.TrimSpace(msg[:maxLen-1]) + "..."
}
