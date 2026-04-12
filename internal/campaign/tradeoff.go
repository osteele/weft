package campaign

import "github.com/osteele/weft/internal/bidding"

func StrategyMatchesTradeoffLabel(strategy bidding.SelectionStrategy, label string, optionCount int) bool {
	if label == "" {
		return false
	}
	if label == "cheap/fast/fastest" {
		return true
	}
	switch strategy {
	case bidding.StrategyCheap:
		return label == "cheap"
	case bidding.StrategyFast:
		return label == "fast"
	case bidding.StrategyFastest:
		return label == "fastest" || (optionCount == 2 && label == "fast")
	default:
		return false
	}
}

func SelectDefaultTradeoff(strategy bidding.SelectionStrategy, options []TradeoffOption) (int, string) {
	if len(options) == 0 {
		return 0, ""
	}
	for i, option := range options {
		if StrategyMatchesTradeoffLabel(strategy, option.Label, len(options)) {
			return i, option.ID
		}
	}
	if strategy == bidding.StrategyFast {
		middle := len(options) / 2
		return middle, options[middle].ID
	}
	return 0, options[0].ID
}
