package ids

import (
	"fmt"
	"strconv"
	"strings"
)

const instanceIDPrefix = "wi"

// FormatInstanceID returns the canonical CLI representation for an instance ID.
func FormatInstanceID(id int64) string {
	return fmt.Sprintf("%s%d", instanceIDPrefix, id)
}

// ParseInstanceID parses a single instance ID token.
// Accepted forms are numeric IDs ("123") and prefixed IDs ("wi123").
func ParseInstanceID(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty instance ID")
	}
	if len(s) >= len(instanceIDPrefix) && strings.EqualFold(s[:len(instanceIDPrefix)], instanceIDPrefix) {
		s = s[len(instanceIDPrefix):]
		if s == "" {
			return 0, fmt.Errorf("missing numeric suffix")
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// FormatInstanceIDList formats instance IDs in canonical form.
func FormatInstanceIDList(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, FormatInstanceID(id))
	}
	return out
}
