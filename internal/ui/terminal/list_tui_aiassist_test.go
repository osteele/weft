package terminal

import (
	"strings"
	"testing"
)

func TestRenderMarkdownBlock_WrapsLongParagraph(t *testing.T) {
	text := "alpha beta gamma delta epsilon zeta eta theta iota kappa"
	lines := renderMarkdownBlock(text, 20)
	if len(lines) < 2 {
		t.Fatalf("expected wrapping into multiple lines, got %d: %v", len(lines), lines)
	}
	for _, line := range lines {
		if displayLen(line) > 20 {
			t.Errorf("line exceeds width: %q (display %d)", line, displayLen(line))
		}
	}
}

func TestRenderMarkdownBlock_BulletAndHeading(t *testing.T) {
	out := renderMarkdownBlock("# heading\n- item one\n- item two", 80)
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "heading") {
		t.Fatalf("heading body missing: %q", joined)
	}
	if !strings.Contains(joined, "•") {
		t.Fatalf("bullet replacement missing: %q", joined)
	}
}

func TestRenderInlineMarkdown_StripsMatchedDelims(t *testing.T) {
	out := renderInlineMarkdown("plain **bold** and `code` here")
	if strings.Contains(out, "**") || strings.Contains(out, "`") {
		t.Fatalf("delimiters not stripped: %q", out)
	}
	for _, want := range []string{"plain", "bold", "and", "code", "here"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %q", want, out)
		}
	}
}

func TestRenderInlineMarkdown_UnmatchedDelimsLeftAlone(t *testing.T) {
	out := renderInlineMarkdown("trailing **unclosed")
	if !strings.Contains(out, "**unclosed") {
		t.Fatalf("unmatched delim should be preserved: %q", out)
	}
}

// displayLen counts runes outside ANSI CSI sequences. Cheap stand-in for
// lipgloss.Width that avoids importing lipgloss into a small unit test.
func displayLen(s string) int {
	n := 0
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			in = true
			i++
			continue
		}
		if in {
			if c == 'm' {
				in = false
			}
			continue
		}
		n++
	}
	return n
}
