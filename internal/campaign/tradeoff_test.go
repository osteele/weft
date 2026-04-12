package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/bidding"
)

func TestSelectDefaultTradeoff_PrefersMiddleForFast(t *testing.T) {
	options := []TradeoffOption{
		{ID: "cheap", Label: "cheap"},
		{ID: "middle"},
		{ID: "fastest", Label: "fastest"},
	}
	cursor, active := SelectDefaultTradeoff(bidding.StrategyFast, options)
	if active != "middle" {
		t.Fatalf("active = %q, want middle", active)
	}
	if cursor != 1 {
		t.Fatalf("cursor = %d, want 1", cursor)
	}
}

func TestSelectDefaultTradeoff_MapsFastestToFastWhenTwoOptions(t *testing.T) {
	options := []TradeoffOption{
		{ID: "cheap", Label: "cheap"},
		{ID: "fast-endpoint", Label: "fast"},
	}
	cursor, active := SelectDefaultTradeoff(bidding.StrategyFastest, options)
	if active != "fast-endpoint" {
		t.Fatalf("active = %q, want fast-endpoint", active)
	}
	if cursor != 1 {
		t.Fatalf("cursor = %d, want 1", cursor)
	}
}
