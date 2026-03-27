package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

type cloudInstanceObservability struct {
	Uptime   *time.Duration
	Cost     *float64
	Rate     *float64
	Terminal bool
}

type cloudInstanceView struct {
	Launch   *db.Launch
	Instance *cloud.Instance
}

type cloudInstanceAggregate struct {
	Count       int
	TotalCost   *float64
	CurrentRate *float64
}

func observeLaunch(ci *db.Launch, inst *cloud.Instance, now time.Time) cloudInstanceObservability {
	obs := cloudInstanceObservability{}
	if ci == nil {
		return obs
	}

	if now.IsZero() {
		now = time.Now()
	}
	obs.Terminal = campaign.IsInstanceTerminal(ci.Status)

	switch {
	case inst != nil && inst.CostPerHour > 0:
		rate := inst.CostPerHour
		obs.Rate = &rate
	case ci.CostPerHourCents > 0:
		rate := float64(ci.CostPerHourCents) / 100.0
		obs.Rate = &rate
	}

	if ci.LaunchedAt != nil {
		launchedAt := time.Unix(*ci.LaunchedAt, 0)
		end := now
		if ci.EndedAt != nil {
			end = time.Unix(*ci.EndedAt, 0)
		}
		if end.Before(launchedAt) {
			end = launchedAt
		}
		uptime := end.Sub(launchedAt).Truncate(time.Second)
		obs.Uptime = &uptime
	}

	if obs.Rate != nil && obs.Uptime != nil {
		cost := obs.Uptime.Hours() * *obs.Rate
		obs.Cost = &cost
	} else if ci.ActualSpendCents > 0 {
		cost := float64(ci.ActualSpendCents) / 100.0
		obs.Cost = &cost
	}

	return obs
}

func (o cloudInstanceObservability) currentRate() float64 {
	if o.Rate == nil || o.Terminal {
		return 0
	}
	return *o.Rate
}

func formatLaunchMetricParts(obs cloudInstanceObservability) []string {
	parts := make([]string, 0, 3)
	if obs.Uptime != nil {
		parts = append(parts, "uptime: "+obs.Uptime.String())
	}
	if obs.Cost != nil {
		parts = append(parts, fmt.Sprintf("cost: $%.2f", *obs.Cost))
	}
	if obs.Rate != nil {
		parts = append(parts, fmt.Sprintf("rate: $%.2f/hr", *obs.Rate))
	}
	return parts
}

func formatLaunchMetricsInline(obs cloudInstanceObservability) string {
	return strings.Join(formatLaunchMetricParts(obs), "  ")
}

func summarizeLaunches(instances []cloudInstanceView, now time.Time) cloudInstanceAggregate {
	agg := cloudInstanceAggregate{}
	for _, view := range instances {
		if view.Launch == nil {
			continue
		}
		agg.Count++
		obs := observeLaunch(view.Launch, view.Instance, now)
		if obs.Cost != nil {
			if agg.TotalCost == nil {
				agg.TotalCost = new(float64)
			}
			*agg.TotalCost += *obs.Cost
		}
		if obs.Rate != nil {
			if agg.CurrentRate == nil {
				agg.CurrentRate = new(float64)
			}
			*agg.CurrentRate += obs.currentRate()
		}
	}
	return agg
}

func formatCloudAggregateSummary(label string, agg cloudInstanceAggregate) string {
	parts := make([]string, 0, 2)
	if agg.TotalCost != nil {
		parts = append(parts, fmt.Sprintf("cost: $%.2f", *agg.TotalCost))
	}
	if agg.CurrentRate != nil {
		parts = append(parts, fmt.Sprintf("current rate: $%.2f/hr", *agg.CurrentRate))
	}
	if len(parts) == 0 {
		return ""
	}
	if label != "" {
		return label + "  " + strings.Join(parts, "  ")
	}
	return strings.Join(parts, "  ")
}
