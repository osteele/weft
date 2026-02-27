package placement

import (
	"fmt"
	"sort"

	"github.com/osteele/weft/internal/vastai"
)

// CloudOffering represents a compute option for running a job,
// either locally (wait for free GPU) or on a cloud provider.
type CloudOffering struct {
	Source       string        // "local" or "vastai"
	DisplayName  string        // e.g., "Free: wait ~25 min" or "Vast.ai RTX 4090 24GB"
	GPUName      string        // e.g., "RTX 3090", "RTX 4090"
	GPUMemGB     float64       // per-GPU memory
	CostPerHour  float64       // $/hr (0 for local)
	EstSetupMin  float64       // estimated setup time in minutes
	EstRunMin    float64       // estimated runtime in minutes
	EstTotalCost float64       // estimated total cost ($)
	EstQueueMin  float64       // estimated queue wait time (local only)
	OfferID      int           // Vast.ai offer ID (0 for local)
	Offer        *vastai.Offer // full offer details (nil for local)
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
	vastOffers []vastai.Offer,
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

	// Vast.ai options
	for i, offer := range vastOffers {
		estSetupMin := estimateSetupMinutes(offer)
		estRunMin := estimateCloudRunMinutes(avgJobMinutes, localDLPerf, offer.DLPerf)
		totalHours := (estSetupMin + estRunMin) / 60.0
		estCost := totalHours * offer.CostPerHour

		off := CloudOffering{
			Source:       "vastai",
			GPUName:      offer.GPUName,
			GPUMemGB:     offer.GPUMemGB,
			CostPerHour:  offer.CostPerHour,
			EstSetupMin:  estSetupMin,
			EstRunMin:    estRunMin,
			EstTotalCost: estCost,
			OfferID:      offer.ID,
			Offer:        &vastOffers[i],
		}
		off.DisplayName = fmt.Sprintf("Vast.ai %s %.0fGB: ~$%.2f (~%.0fm setup + ~%.0fm run)",
			offer.GPUName, offer.GPUMemGB, estCost, estSetupMin, estRunMin)
		offerings = append(offerings, off)
	}

	// Sort: local first, then cloud by ascending total cost
	sort.Slice(offerings[1:], func(i, j int) bool {
		return offerings[1+i].EstTotalCost < offerings[1+j].EstTotalCost
	})

	return offerings
}

// estimateSetupMinutes estimates how long it takes to get a Vast.ai instance ready.
// Includes spin-up time (~0.75 min) plus estimated rsync time based on bandwidth.
func estimateSetupMinutes(offer vastai.Offer) float64 {
	spinUp := 0.75 // 45 seconds typical
	// Assume ~500MB working directory to sync
	workDirMB := 500.0
	// Download bandwidth is in Mbps, convert to MB/s (divide by 8)
	bwMBps := offer.DownloadBandwidth / 8.0
	if bwMBps < 1 {
		bwMBps = 1
	}
	syncMin := (workDirMB / bwMBps) / 60.0
	return spinUp + syncMin
}

// estimateCloudRunMinutes scales local runtime by GPU performance ratio.
// If perf data is unavailable, assumes the cloud GPU is roughly equivalent.
func estimateCloudRunMinutes(localRunMin, localDLPerf, cloudDLPerf float64) float64 {
	if localDLPerf <= 0 || cloudDLPerf <= 0 {
		return localRunMin // no perf data, assume equivalent
	}
	// Higher DLPerf = faster, so scale inversely
	return localRunMin * (localDLPerf / cloudDLPerf)
}
