package placement

import (
	"fmt"
	"sort"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/estimate"
)

// CloudOffering represents a compute option for running a job,
// either locally (wait for free GPU) or on a cloud provider.
type CloudOffering struct {
	Source           string       // "local", "vastai", "runpod"
	DisplayName      string       // e.g., "Free: wait ~25 min" or "Vast.ai RTX 4090 24GB"
	GPUName          string       // e.g., "RTX 3090", "RTX 4090"
	GPUMemGB         float64      // per-GPU memory
	CostPerHour      float64      // $/hr (0 for local)
	EstSetupMin      float64      // estimated setup time in minutes
	EstRunMin        float64      // estimated runtime in minutes
	EstTotalCost     float64      // estimated total cost ($)
	EstQueueMin      float64      // estimated queue wait time (local only)
	OfferID          string       // provider-specific offer ID (empty for local)
	Offer            *cloud.Offer // full offer details (nil for local)
	SurvivalProb     float64      // 0-1, survival probability (0 if no model)
	RiskAdjustedCost float64      // expected cost including retry risk (0 if no model)
}

// BuildCloudOfferings creates a sorted list of compute options for a job.
// localGPUName is the GPU on the host where the job is queued.
// queueDepth is how many jobs are ahead in the queue.
// avgJobMinutes is the average recent job runtime in minutes.
// localDLPerf is the deep-learning perf score of the local GPU (for scaling).
func BuildCloudOfferings(
	localGPUName string,
	localGPUMemGB float64,
	queueDepth int,
	avgJobMinutes float64,
	localDLPerf float64,
	cloudOffers []cloud.Offer,
) []CloudOffering {
	var offerings []CloudOffering

	// Local option: wait for free GPU
	estQueueMin := float64(queueDepth) * avgJobMinutes
	localOff := CloudOffering{
		Source:       "local",
		GPUName:      localGPUName,
		GPUMemGB:     localGPUMemGB,
		EstQueueMin:  estQueueMin,
		EstRunMin:    avgJobMinutes,
		EstTotalCost: 0,
	}
	localOff.DisplayName = fmt.Sprintf("Free: wait ~%.0f min (queue: ~%.0f min + run: ~%.0f min)",
		estQueueMin+avgJobMinutes, estQueueMin, avgJobMinutes)
	offerings = append(offerings, localOff)

	// Cloud options
	for i, offer := range cloudOffers {
		startup := estimate.EstimateStartup(string(offer.Provider))
		bwBps := cloud.MbpsToBytesPerSec(offer.DownloadBandwidth)
		provision := estimate.EstimateProvision(estimate.ProvisionInput{
			BandwidthBytesPerSec: bwBps,
		})
		estSetupMin := (startup.Mean + provision.Mean).Minutes()

		localEst := estimate.Constant(estimate.DurFromMinutes(avgJobMinutes))
		cloudRunEst := estimate.EstimateCloudRuntime(localEst, localDLPerf, offer.DLPerf)
		estRunMin := cloudRunEst.Mean.Minutes()

		totalHours := (estSetupMin + estRunMin) / 60.0
		estCost := totalHours * offer.CostPerHour

		off := CloudOffering{
			Source:       string(offer.Provider),
			GPUName:      offer.GPUName,
			GPUMemGB:     offer.GPUMemGB,
			CostPerHour:  offer.CostPerHour,
			EstSetupMin:  estSetupMin,
			EstRunMin:    estRunMin,
			EstTotalCost: estCost,
			OfferID:      offer.ProviderID,
			Offer:        &cloudOffers[i],
		}
		providerLabel := string(offer.Provider)
		if providerLabel == "" {
			providerLabel = "rental"
		}
		if off.SurvivalProb > 0 {
			off.DisplayName = fmt.Sprintf("%s %s %.0fGB: ~$%.2f (%.0f%% surv, ~%.0fm)",
				providerLabel, offer.GPUName, offer.GPUMemGB, estCost, off.SurvivalProb*100, estSetupMin+estRunMin)
		} else {
			off.DisplayName = fmt.Sprintf("%s %s %.0fGB: ~$%.2f (~%.0fm setup + ~%.0fm run)",
				providerLabel, offer.GPUName, offer.GPUMemGB, estCost, estSetupMin, estRunMin)
		}
		offerings = append(offerings, off)
	}

	// Sort: local first, then cloud by ascending cost (risk-adjusted when available)
	sort.Slice(offerings[1:], func(i, j int) bool {
		ci, cj := offerings[1+i], offerings[1+j]
		costI, costJ := ci.EstTotalCost, cj.EstTotalCost
		if ci.RiskAdjustedCost > 0 {
			costI = ci.RiskAdjustedCost
		}
		if cj.RiskAdjustedCost > 0 {
			costJ = cj.RiskAdjustedCost
		}
		return costI < costJ
	})

	return offerings
}
