package util

// Ellipsis is the canonical truncation marker. Use this everywhere instead
// of literal "..." or "…" so the codebase stays visually consistent.
const Ellipsis = "…"

// Truncate returns s shortened to at most max bytes, with Ellipsis appended
// in place of the trailing characters when truncation occurs. For pathological
// max values (<= 0) it returns the empty string; if max is too small to fit
// the marker, it returns a marker-free hard slice.
//
// Counts bytes, not runes or display width — fine for ASCII labels and error
// strings. For width-aware truncation in the terminal UI, use
// terminal.truncateDisplayWidth.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	if max <= len(Ellipsis) {
		return s[:max]
	}
	return s[:max-len(Ellipsis)] + Ellipsis
}
