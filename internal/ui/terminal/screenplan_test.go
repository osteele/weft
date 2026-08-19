package terminal

import "testing"

func TestScreenPlanRenderJoinsLines(t *testing.T) {
	p := screenPlan{
		width: 10,
		lines: []screenLine{
			{text: "one"},
			{text: ""},
			{text: "three"},
		},
	}
	if got, want := p.render(), "one\n\nthree"; got != want {
		t.Fatalf("render() = %q, want %q", got, want)
	}
}

func TestScreenPlanHit(t *testing.T) {
	lineTarget := clickTarget{kind: targetSelectRow, rowIdx: 3}
	wideTarget := clickTarget{kind: targetSelectRow, rowIdx: 9}
	narrowTarget := clickTarget{kind: targetOpenURL, url: "https://example.test"}
	p := screenPlan{
		width: 40,
		lines: []screenLine{
			{text: "row one", lineTarget: lineTarget},
			{text: "status", spans: []hitSpan{
				{startCol: 0, endCol: 40, target: wideTarget},
				{startCol: 7, endCol: 13, target: narrowTarget},
			}},
			{text: "plain"},
		},
	}

	if tgt, ok := p.hit(0, 0); !ok || tgt != lineTarget {
		t.Fatalf("hit(0,0) = %+v, %v; want line target", tgt, ok)
	}
	if tgt, ok := p.hit(10, 1); !ok || tgt != narrowTarget {
		t.Fatalf("hit(10,1) = %+v, %v; want narrowest containing span", tgt, ok)
	}
	if tgt, ok := p.hit(1, 1); !ok || tgt != wideTarget {
		t.Fatalf("hit(1,1) = %+v, %v; want wide span outside narrow span", tgt, ok)
	}
	if _, ok := p.hit(0, 2); ok {
		t.Fatal("hit on line without target should miss")
	}

	for _, pt := range [][2]int{{0, -1}, {0, 3}, {-1, 0}, {40, 0}} {
		if _, ok := p.hit(pt[0], pt[1]); ok {
			t.Fatalf("hit(%d,%d) outside plan bounds should miss", pt[0], pt[1])
		}
	}
}

func TestScreenPlanHitPicksNarrowestOfOverlappingSpans(t *testing.T) {
	outer := clickTarget{kind: targetSelectRow, rowIdx: 1}
	middle := clickTarget{kind: targetToggleSection, toggle: "mid"}
	inner := clickTarget{kind: targetRestartDaemon}
	p := screenPlan{
		width: 30,
		lines: []screenLine{
			{
				text: "nested",
				spans: []hitSpan{
					{startCol: 0, endCol: 30, target: outer},
					{startCol: 4, endCol: 20, target: middle},
					{startCol: 8, endCol: 12, target: inner},
				},
			},
		},
	}
	if tgt, ok := p.hit(10, 0); !ok || tgt != inner {
		t.Fatalf("hit(10,0) = %+v, %v; want innermost span", tgt, ok)
	}
	if tgt, ok := p.hit(6, 0); !ok || tgt != middle {
		t.Fatalf("hit(6,0) = %+v, %v; want middle span", tgt, ok)
	}
	if tgt, ok := p.hit(25, 0); !ok || tgt != outer {
		t.Fatalf("hit(25,0) = %+v, %v; want outer span", tgt, ok)
	}
}

func TestScreenPlanHitGapBetweenAdjacentSpans(t *testing.T) {
	left := clickTarget{kind: targetSelectRow, rowIdx: 1}
	right := clickTarget{kind: targetSelectRow, rowIdx: 2}
	p := screenPlan{
		width: 20,
		lines: []screenLine{
			{
				text: "split",
				spans: []hitSpan{
					{startCol: 0, endCol: 5, target: left},
					{startCol: 10, endCol: 15, target: right},
				},
			},
		},
	}
	if tgt, ok := p.hit(4, 0); !ok || tgt != left {
		t.Fatalf("hit(4,0) = %+v, %v; want left span", tgt, ok)
	}
	if _, ok := p.hit(7, 0); ok {
		t.Fatal("column between disjoint spans should miss")
	}
	if tgt, ok := p.hit(14, 0); !ok || tgt != right {
		t.Fatalf("hit(14,0) = %+v, %v; want right span", tgt, ok)
	}
	if _, ok := p.hit(5, 0); ok {
		t.Fatal("end-exclusive span boundary should miss")
	}
}
