package terminal

import "github.com/osteele/weft/internal/util"

func truncate(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return util.Truncate(s, maxLen)
}
