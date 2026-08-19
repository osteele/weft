package hit

import "testing"

func TestPlanRenderJoinsLines(t *testing.T) {
	p := Plan[string]{
		Width: 10,
		Lines: []Line[string]{
			{Text: "one"},
			{Text: ""},
			{Text: "three"},
		},
	}
	if got, want := p.Render(), "one\n\nthree"; got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}

func TestPlanHit(t *testing.T) {
	p := Plan[string]{
		Width: 40,
		Lines: []Line[string]{
			{Text: "row one", LineTarget: "line"},
			{Text: "status", Spans: []Span[string]{
				{StartCol: 0, EndCol: 40, Target: "wide"},
				{StartCol: 7, EndCol: 13, Target: "narrow"},
			}},
			{Text: "plain"},
		},
	}

	if tgt, ok := p.Hit(0, 0); !ok || tgt != "line" {
		t.Fatalf("Hit(0,0) = %q, %v; want line target", tgt, ok)
	}
	if tgt, ok := p.Hit(10, 1); !ok || tgt != "narrow" {
		t.Fatalf("Hit(10,1) = %q, %v; want narrowest containing span", tgt, ok)
	}
	if tgt, ok := p.Hit(1, 1); !ok || tgt != "wide" {
		t.Fatalf("Hit(1,1) = %q, %v; want wide span outside narrow span", tgt, ok)
	}
	if _, ok := p.Hit(0, 2); ok {
		t.Fatal("Hit on line without target should miss")
	}

	for _, pt := range [][2]int{{0, -1}, {0, 3}, {-1, 0}, {40, 0}} {
		if _, ok := p.Hit(pt[0], pt[1]); ok {
			t.Fatalf("Hit(%d,%d) outside plan bounds should miss", pt[0], pt[1])
		}
	}
}

func TestPlanHitPicksNarrowestOfOverlappingSpans(t *testing.T) {
	p := Plan[string]{
		Width: 30,
		Lines: []Line[string]{
			{
				Text: "nested",
				Spans: []Span[string]{
					{StartCol: 0, EndCol: 30, Target: "outer"},
					{StartCol: 4, EndCol: 20, Target: "middle"},
					{StartCol: 8, EndCol: 12, Target: "inner"},
				},
			},
		},
	}
	if tgt, ok := p.Hit(10, 0); !ok || tgt != "inner" {
		t.Fatalf("Hit(10,0) = %q, %v; want innermost span", tgt, ok)
	}
	if tgt, ok := p.Hit(6, 0); !ok || tgt != "middle" {
		t.Fatalf("Hit(6,0) = %q, %v; want middle span", tgt, ok)
	}
	if tgt, ok := p.Hit(25, 0); !ok || tgt != "outer" {
		t.Fatalf("Hit(25,0) = %q, %v; want outer span", tgt, ok)
	}
}

func TestPlanHitGapBetweenAdjacentSpans(t *testing.T) {
	p := Plan[string]{
		Width: 20,
		Lines: []Line[string]{
			{
				Text: "split",
				Spans: []Span[string]{
					{StartCol: 0, EndCol: 5, Target: "left"},
					{StartCol: 10, EndCol: 15, Target: "right"},
				},
			},
		},
	}
	if tgt, ok := p.Hit(4, 0); !ok || tgt != "left" {
		t.Fatalf("Hit(4,0) = %q, %v; want left span", tgt, ok)
	}
	if _, ok := p.Hit(7, 0); ok {
		t.Fatal("column between disjoint spans should miss")
	}
	if tgt, ok := p.Hit(14, 0); !ok || tgt != "right" {
		t.Fatalf("Hit(14,0) = %q, %v; want right span", tgt, ok)
	}
	if _, ok := p.Hit(5, 0); ok {
		t.Fatal("end-exclusive span boundary should miss")
	}
}

func TestPlanAddAppendsRowsAndSpans(t *testing.T) {
	p := New[string](80, 24)
	if cap(p.Lines) != 24 {
		t.Fatalf("New(80,24) lines capacity = %d, want 24", cap(p.Lines))
	}
	p.Add("row", 3, "select")
	p.AddSpan(Span[string]{StartCol: 0, EndCol: 3, Target: "copy"})
	if len(p.Lines) != 1 {
		t.Fatalf("Add appended %d lines, want 1", len(p.Lines))
	}
	line := p.Lines[0]
	if line.Text != "row" || line.RowIdx != 3 || line.LineTarget != "select" {
		t.Fatalf("line = %+v, want text=row rowIdx=3 target=select", line)
	}
	if got := p.RowIdxAt(0); got != 3 {
		t.Fatalf("RowIdxAt(0) = %d, want 3", got)
	}
	if got := p.RowIdxAt(1); got != -1 {
		t.Fatalf("RowIdxAt(1) = %d, want -1 off screen", got)
	}
	if tgt, ok := p.Hit(1, 0); !ok || tgt != "copy" {
		t.Fatalf("Hit(1,0) = %q, %v; want span target", tgt, ok)
	}
}
