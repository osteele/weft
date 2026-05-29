package dashtabs

import "testing"

func testViews() []View {
	return []View{
		newPulseView(),    // "1 Pulse"     (7 chars + 2 padding = 9)
		newTimelineView(), // "2 Timeline"  (10 + 2 = 12)
		newFleetView(),    // "3 Fleet"     (7 + 2 = 9)
		newFocusView(),    // "4 Focus"     (7 + 2 = 9)
		newTreeView(),     // "5 Tree"      (6 + 2 = 8)
		newAlertsView(),   // "6 Alerts"    (8 + 2 = 10)
		newHistoryView(),  // "7 History"   (9 + 2 = 11)
		newSankeyView(),   // "8 Flow"      (6 + 2 = 8)
		newCostView(),     // "9 Cost"      (6 + 2 = 8)
		newUsageView(),    // "0 Usage"     (7 + 2 = 9)
	}
}

func TestEvenSplitNoOrphans(t *testing.T) {
	// 10 tabs over 2 rows: 5/5.
	got := evenSplit(10, 2)
	if len(got) != 2 || len(got[0]) != 5 || len(got[1]) != 5 {
		t.Fatalf("10/2 split = %v, want [[0..4][5..9]]", got)
	}
	// 11 tabs over 2 rows: 6/5 (extra goes to leading row, not trailing).
	got = evenSplit(11, 2)
	if len(got) != 2 || len(got[0]) != 6 || len(got[1]) != 5 {
		t.Fatalf("11/2 split = %v, want 6+5", got)
	}
	// 10 over 3: 4/3/3 (leading row gets the extra; trailing rows are equal).
	got = evenSplit(10, 3)
	if len(got[0]) != 4 || len(got[1]) != 3 || len(got[2]) != 3 {
		t.Fatalf("10/3 split = %v, want 4+3+3", got)
	}
}

func TestTabRowLayoutSingleLineWhenItFits(t *testing.T) {
	views := testViews()
	// Width well in excess of the single-line width → single row.
	rows := tabRowLayout(views, 200)
	if len(rows) != 1 || len(rows[0]) != 10 {
		t.Fatalf("wide layout = %v, want one row of 10", rows)
	}
}

func TestTabRowLayoutWrapsToTwoLines(t *testing.T) {
	views := testViews()
	// Compute the single-line width and shrink by 1 to force a wrap.
	singleW := 0
	for i, v := range views {
		if i > 0 {
			singleW++
		}
		singleW += tabLabelWidth(v)
	}
	rows := tabRowLayout(views, singleW-1)
	if len(rows) != 2 {
		t.Fatalf("narrow layout = %d rows, want 2", len(rows))
	}
	// Even split: 5 + 5.
	if len(rows[0]) != 5 || len(rows[1]) != 5 {
		t.Errorf("narrow split = %d + %d, want 5 + 5", len(rows[0]), len(rows[1]))
	}
}

func TestNeighborInRowWrapsAtRowEnds(t *testing.T) {
	rows := [][]int{{0, 1, 2, 3, 4}, {5, 6, 7, 8, 9}}
	// Right from last on row → first of same row.
	if got := neighborInRow(rows, 4, +1); got != 0 {
		t.Errorf("right from 4 (end of row 0) = %d, want 0", got)
	}
	// Left from first on row → last of same row.
	if got := neighborInRow(rows, 5, -1); got != 9 {
		t.Errorf("left from 5 (start of row 1) = %d, want 9", got)
	}
}

func TestNeighborInAdjacentRowNearestCenter(t *testing.T) {
	views := testViews()
	rows := tabRowLayout(views, 60) // forces 2 rows
	if len(rows) != 2 {
		t.Skipf("layout did not produce 2 rows: %v", rows)
	}
	// Pick a tab in the middle of row 0 and move down. Result should be in row 1.
	from := rows[0][len(rows[0])/2]
	got := neighborInAdjacentRow(views, rows, from, +1)
	// Verify result is on row 1.
	foundOnRow := -1
	for r, row := range rows {
		for _, idx := range row {
			if idx == got {
				foundOnRow = r
			}
		}
	}
	if foundOnRow != 1 {
		t.Errorf("down from row 0 landed on row %d, want row 1", foundOnRow)
	}

	// Going down then up should return to a tab on the same row as the
	// original, ideally the same column. Test that up-from-down lands on
	// row 0.
	back := neighborInAdjacentRow(views, rows, got, -1)
	for r, row := range rows {
		for _, idx := range row {
			if idx == back && r != 0 {
				t.Errorf("up after down landed on row %d, want row 0", r)
			}
		}
	}
}

func TestFlowVsFleetAreDistinct(t *testing.T) {
	// With full labels there's no abbreviation collision, but sanity check
	// the tab titles remain distinct.
	if newFleetView().Title() == newSankeyView().Title() {
		t.Fatal("Fleet and Flow share Title()")
	}
}
