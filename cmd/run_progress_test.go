package cmd

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestRunPhaseRecorderBannerAndSummary(t *testing.T) {
	var buf bytes.Buffer
	rec := newRunPhaseRecorder(&buf, "uv run hello.py")

	end1 := rec.Phase("predictor", "checking predictor")
	end1()
	end2 := rec.Phase("submit", "submitting job")
	end2()
	rec.PrintSummary()

	out := buf.String()
	if !strings.HasPrefix(out, "weft run: uv run hello.py\n") {
		t.Fatalf("missing banner; got:\n%s", out)
	}
	wantLines := []string{
		`weft run: checking predictor… \(\d+\.\d+s\)`,
		`weft run: submitting job… \(\d+\.\d+s\)`,
		`weft run: predictor \d+\.\d+s, submit \d+\.\d+s, total \d+\.\d+s`,
	}
	for _, pat := range wantLines {
		if !regexp.MustCompile(pat).MatchString(out) {
			t.Errorf("expected line matching %q in output:\n%s", pat, out)
		}
	}
}

func TestRunPhaseRecorderSummaryIdempotent(t *testing.T) {
	var buf bytes.Buffer
	rec := newRunPhaseRecorder(&buf, "x")
	rec.Phase("a", "doing a")()
	rec.PrintSummary()
	first := buf.Len()
	rec.PrintSummary()
	if buf.Len() != first {
		t.Fatalf("PrintSummary should be idempotent; second call wrote more")
	}
}

func TestRunPhaseRecorderEvent(t *testing.T) {
	var buf bytes.Buffer
	rec := newRunPhaseRecorder(&buf, "x")
	rec.Event("sync skipped (--no-sync) — will sync on next background sync")
	out := buf.String()
	if !regexp.MustCompile(`weft run: sync skipped \(--no-sync\) — will sync on next background sync \(\d+\.\d+s\)`).MatchString(out) {
		t.Fatalf("Event output missing or malformed:\n%s", out)
	}
}

func TestRunPhaseRecorderNoPhases(t *testing.T) {
	var buf bytes.Buffer
	rec := newRunPhaseRecorder(&buf, "x")
	rec.PrintSummary()
	out := buf.String()
	if !regexp.MustCompile(`weft run: total \d+\.\d+s\n`).MatchString(out) {
		t.Fatalf("expected total-only summary; got:\n%s", out)
	}
}

func TestRunPhaseRecorderEmptyBanner(t *testing.T) {
	var buf bytes.Buffer
	_ = newRunPhaseRecorder(&buf, "   ")
	if got := buf.String(); got != "weft run\n" {
		t.Fatalf("empty command should produce bare banner; got %q", got)
	}
}

func TestRunPhaseRecorderNilSafe(t *testing.T) {
	var r *runPhaseRecorder
	end := r.Phase("k", "doing k")
	end()
	r.Event("x")
	r.PrintSummary()
}
