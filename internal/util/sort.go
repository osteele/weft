package util

import (
	"sort"
	"strconv"
	"strings"
)

// NaturalSortStrings sorts strings using natural ordering where numeric segments
// are compared as numbers (e.g., "host30" < "host100").
func NaturalSortStrings(s []string) {
	sort.Slice(s, func(i, j int) bool {
		return NaturalLess(s[i], s[j])
	})
}

// NaturalLess compares two strings using natural ordering.
func NaturalLess(a, b string) bool {
	aParts := splitIntoSegments(a)
	bParts := splitIntoSegments(b)

	minLen := min(len(aParts), len(bParts))

	for i := 0; i < minLen; i++ {
		aSeg := aParts[i]
		bSeg := bParts[i]
		aNum, aIsNum := parseNumber(aSeg)
		bNum, bIsNum := parseNumber(bSeg)

		if aIsNum && bIsNum {
			if aNum != bNum {
				return aNum < bNum
			}
			continue
		}

		aLower := strings.ToLower(aSeg)
		bLower := strings.ToLower(bSeg)
		if aLower != bLower {
			return aLower < bLower
		}
	}

	if len(aParts) != len(bParts) {
		return len(aParts) < len(bParts)
	}
	return a < b
}

func splitIntoSegments(s string) []string {
	var segments []string
	var current strings.Builder
	var lastRune rune

	for _, r := range s {
		isDigit := r >= '0' && r <= '9'
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')

		if current.Len() == 0 {
			current.WriteRune(r)
			lastRune = r
			continue
		}

		lastIsDigit := lastRune >= '0' && lastRune <= '9'
		lastIsAlpha := (lastRune >= 'a' && lastRune <= 'z') || (lastRune >= 'A' && lastRune <= 'Z')
		sameType := (isDigit && lastIsDigit) || (isAlpha && lastIsAlpha) ||
			(!isDigit && !isAlpha && !lastIsDigit && !lastIsAlpha)

		if sameType {
			current.WriteRune(r)
			lastRune = r
			continue
		}

		segments = append(segments, current.String())
		current.Reset()
		current.WriteRune(r)
		lastRune = r
	}

	if current.Len() > 0 {
		segments = append(segments, current.String())
	}

	return segments
}

func parseNumber(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}
