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
	HasUptime bool
	Uptime    time.Duration
	HasCost   bool
	Cost      float64
	HasRate   bool
	Rate      float64
	Terminal  bool
}

type cloudInstanceView struct {
	CloudInstance *db.CloudInstance
	Instance      *cloud.Instance
}

type cloudInstanceAggregate struct {
	Count       int
	HasCost     bool
	TotalCost   float64
	HasRate     bool
	CurrentRate float64
}

func observeCloudInstance(ci *db.CloudInstance, inst *cloud.Instance, now time.Time) cloudInstanceObservability {
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
		obs.HasRate = true
		obs.Rate = inst.CostPerHour
	case ci.CostPerHourCents > 0:
		obs.HasRate = true
		obs.Rate = float64(ci.CostPerHourCents) / 100.0
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
		obs.HasUptime = true
		obs.Uptime = end.Sub(launchedAt).Truncate(time.Second)
	}

	if obs.HasRate && obs.HasUptime {
		obs.HasCost = true
		obs.Cost = obs.Uptime.Hours() * obs.Rate
	} else if ci.ActualSpendCents > 0 {
		obs.HasCost = true
		obs.Cost = float64(ci.ActualSpendCents) / 100.0
	}

	return obs
}

func (o cloudInstanceObservability) currentRate() float64 {
	if !o.HasRate || o.Terminal {
		return 0
	}
	return o.Rate
}

func formatCloudInstanceMetricParts(obs cloudInstanceObservability) []string {
	parts := make([]string, 0, 3)
	if obs.HasUptime {
		parts = append(parts, "uptime: "+obs.Uptime.String())
	}
	if obs.HasCost {
		parts = append(parts, fmt.Sprintf("cost: $%.2f", obs.Cost))
	}
	if obs.HasRate {
		parts = append(parts, fmt.Sprintf("rate: $%.2f/hr", obs.Rate))
	}
	return parts
}

func formatCloudInstanceMetricsInline(obs cloudInstanceObservability) string {
	return strings.Join(formatCloudInstanceMetricParts(obs), "  ")
}

func summarizeCloudInstances(instances []cloudInstanceView, now time.Time) cloudInstanceAggregate {
	agg := cloudInstanceAggregate{}
	for _, view := range instances {
		if view.CloudInstance == nil {
			continue
		}
		agg.Count++
		obs := observeCloudInstance(view.CloudInstance, view.Instance, now)
		if obs.HasCost {
			agg.HasCost = true
			agg.TotalCost += obs.Cost
		}
		if obs.HasRate {
			agg.HasRate = true
			agg.CurrentRate += obs.currentRate()
		}
	}
	return agg
}

func formatCloudAggregateSummary(label string, agg cloudInstanceAggregate) string {
	parts := make([]string, 0, 2)
	if agg.HasCost {
		parts = append(parts, fmt.Sprintf("cost: $%.2f", agg.TotalCost))
	}
	if agg.HasRate {
		parts = append(parts, fmt.Sprintf("current rate: $%.2f/hr", agg.CurrentRate))
	}
	if len(parts) == 0 {
		return ""
	}
	if label != "" {
		return label + "  " + strings.Join(parts, "  ")
	}
	return strings.Join(parts, "  ")
}
