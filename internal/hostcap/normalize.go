// Package hostcap defines shared host-capability label handling.
package hostcap

import (
	"fmt"
	"strings"
)

// Normalize returns lowercase, whitespace-free, de-duplicated capability
// labels, optionally adding the selected agent's agent:<name> label.
func Normalize(agent string, capabilities []string) ([]string, error) {
	labels := append([]string(nil), capabilities...)
	if name := strings.ToLower(strings.TrimSpace(agent)); name != "" {
		name = strings.TrimPrefix(name, "agent:")
		labels = append(labels, "agent:"+name)
	}

	seen := make(map[string]struct{}, len(labels))
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" || strings.ContainsAny(label, "\r\n\t ") {
			return nil, fmt.Errorf("invalid empty or whitespace-containing host capability %q", label)
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		result = append(result, label)
	}
	return result, nil
}
