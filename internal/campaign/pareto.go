package campaign

import (
	"math"
	"sort"
)

// TradeoffOption is a selectable Pareto-frontier plan in the launch TUI.
type TradeoffOption struct {
	ID    string
	Label string
}

type tradeoffPoint struct {
	id   string
	time float64
	cost float64
}

// BuildParetoTradeoffOptions reduces a sampled plan set to non-dominated
// tradeoff points and labels the frontier endpoints. Options are sorted from
// cheapest to fastest.
func BuildParetoTradeoffOptions(plans map[string]StrategyPlan) []TradeoffOption {
	points := summarizeTradeoffPlans(plans)
	if len(points) == 0 {
		return nil
	}

	points = dedupeTradeoffPoints(points)
	points = paretoFrontier(points)
	sort.Slice(points, func(i, j int) bool {
		if !almostEqual(points[i].cost, points[j].cost) {
			return points[i].cost < points[j].cost
		}
		if !almostEqual(points[i].time, points[j].time) {
			return points[i].time > points[j].time
		}
		return points[i].id < points[j].id
	})

	options := make([]TradeoffOption, len(points))
	for i, point := range points {
		options[i] = TradeoffOption{ID: point.id}
	}

	switch len(options) {
	case 1:
		options[0].Label = "cheap/fast/fastest"
	case 2:
		options[0].Label = "cheap"
		options[1].Label = "fast"
	default:
		options[0].Label = "cheap"
		options[len(options)-1].Label = "fastest"
		options[len(options)/2].Label = "fast"
	}

	return options
}

func summarizeTradeoffPlans(plans map[string]StrategyPlan) []tradeoffPoint {
	points := make([]tradeoffPoint, 0, len(plans))
	for id, plan := range plans {
		summary := summarizeTradeoffPlan(plan)
		if summary == nil {
			continue
		}
		points = append(points, tradeoffPoint{
			id:   id,
			time: summary.MaxTime.Mean.Hours(),
			cost: summary.TotalCost,
		})
	}
	return points
}

func summarizeTradeoffPlan(plan StrategyPlan) *StrategySummaryRow {
	if len(plan.ActualEstimates) > 0 {
		return SummarizeTradeoffExecutionEstimates(plan.ActualEstimates)
	}
	return SummarizeTradeoffExecutionEstimates(plan.DisplayEstimates)
}

func dedupeTradeoffPoints(points []tradeoffPoint) []tradeoffPoint {
	sort.Slice(points, func(i, j int) bool {
		if !almostEqual(points[i].cost, points[j].cost) {
			return points[i].cost < points[j].cost
		}
		if !almostEqual(points[i].time, points[j].time) {
			return points[i].time < points[j].time
		}
		return points[i].id < points[j].id
	})

	var deduped []tradeoffPoint
	for _, point := range points {
		if len(deduped) == 0 {
			deduped = append(deduped, point)
			continue
		}
		prev := deduped[len(deduped)-1]
		if almostEqual(prev.cost, point.cost) && almostEqual(prev.time, point.time) {
			continue
		}
		deduped = append(deduped, point)
	}
	return deduped
}

func paretoFrontier(points []tradeoffPoint) []tradeoffPoint {
	var frontier []tradeoffPoint
	for i, candidate := range points {
		dominated := false
		for j, other := range points {
			if i == j {
				continue
			}
			if dominatesTradeoffPoint(other, candidate) {
				dominated = true
				break
			}
		}
		if !dominated {
			frontier = append(frontier, candidate)
		}
	}
	return frontier
}

func dominatesTradeoffPoint(a, b tradeoffPoint) bool {
	betterCost := a.cost < b.cost && !almostEqual(a.cost, b.cost)
	betterTime := a.time < b.time && !almostEqual(a.time, b.time)
	noWorseCost := a.cost < b.cost || almostEqual(a.cost, b.cost)
	noWorseTime := a.time < b.time || almostEqual(a.time, b.time)
	return noWorseCost && noWorseTime && (betterCost || betterTime)
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9
}
