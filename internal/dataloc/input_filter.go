package dataloc

import "strings"

// FilterAutoDetectedInputs applies the safety policy for inferred data inputs.
// Explicit declarations are authoritative; inferred HF refs are only kept when
// no inputs were declared and the inferred ref has an org/name repo shape.
func FilterAutoDetectedInputs(detected, declared []string) []string {
	if hasDeclaredInput(declared) {
		return nil
	}
	out := make([]string, 0, len(detected))
	seen := make(map[string]struct{}, len(detected))
	for _, input := range detected {
		input = strings.TrimSpace(input)
		if input == "" || !autoDetectedInputAllowed(input, declared) {
			continue
		}
		if _, ok := seen[input]; ok {
			continue
		}
		seen[input] = struct{}{}
		out = append(out, input)
	}
	return out
}

func hasDeclaredInput(inputs []string) bool {
	for _, input := range inputs {
		if strings.TrimSpace(input) != "" {
			return true
		}
	}
	return false
}

func autoDetectedInputAllowed(input string, declared []string) bool {
	id, ok := hfInputID(input)
	if !ok {
		return true
	}
	if strings.Count(id, "/") != 1 {
		return false
	}
	lowerID := strings.ToLower(id)
	for _, declaredInput := range declared {
		declaredID, ok := hfInputID(declaredInput)
		if !ok {
			continue
		}
		if strings.Contains(strings.ToLower(declaredID), lowerID) {
			return false
		}
	}
	return true
}

func hfInputID(input string) (string, bool) {
	switch {
	case strings.HasPrefix(input, "hf:"):
		return strings.TrimPrefix(input, "hf:"), true
	case strings.HasPrefix(input, "hf-dataset:"):
		return strings.TrimPrefix(input, "hf-dataset:"), true
	default:
		return "", false
	}
}
