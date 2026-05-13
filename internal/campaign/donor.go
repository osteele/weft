package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/retry"
	"github.com/osteele/weft/internal/transferbw"
)

// DonorConfig holds the selected donor offer and associated metadata.
type DonorConfig struct {
	Offer      cloud.Offer
	DataCenter string
	HFModels   []string // collected from jobs' --input hf:* declarations
	HFDatasets []string // collected from jobs' --input hf-dataset:* declarations
	SourceDirs []string // unique project dirs for uv sync
}

// dcWorker bundles a worker's offer and cost estimate for per-DC donor analysis.
type dcWorker struct {
	offerIdx int
	offer    cloud.Offer
	estimate CostEstimate
}

// estimatedLANCopyRate is the assumed LAN copy speed for vastai copy within
// a data center. Based on observed rates from llm-performance-models seed pattern.
const estimatedLANCopyRate = 500e6 // 500 MB/s

// FindDonorOffer selects a cheap donor instance for the data center that has the
// most workers, when the economics justify it. Only workers in the same data center
// as the donor participate in the call tree — copies don't work across DCs.
//
// The cost comparison per DC is:
//
//	WITHOUT donor: workerRate × downloadTime  (per worker)
//	WITH donor:    donorRate × (downloadTime + copyTime) + workerRate × copyTime  (per worker)
//
// The donor runs during both download and copy phases. Each worker pays GPU rates
// only during copy (which replaces the download it would otherwise do).
//
// Returns nil if no suitable donor offer is found or if a donor would cost more.
func FindDonorOffer(client cloud.Client, workerOffers []cloud.Offer, estimates []CostEstimate, groups []InstanceGroup) (*DonorConfig, error) {
	if len(workerOffers) < 2 || len(estimates) == 0 {
		return nil, nil
	}

	// Group workers by data center
	dcWorkers := make(map[string][]dcWorker)
	for i, o := range workerOffers {
		if o.DataCenter == "" {
			continue
		}
		var est CostEstimate
		if i < len(estimates) {
			est = estimates[i]
		}
		dcWorkers[o.DataCenter] = append(dcWorkers[o.DataCenter], dcWorker{
			offerIdx: i,
			offer:    o,
			estimate: est,
		})
	}

	// Search for cheap single-GPU donor offers (one search, filter per DC)
	minReliability := 0.95
	if cfg, err := config.Load(); err == nil && cfg != nil {
		minReliability = cfg.CampaignReliability()
	}
	donorOffers, err := client.SearchOffers(cloud.OfferConstraints{
		NumGPUs:        1,
		MinReliability: minReliability,
	})
	if err != nil {
		return nil, fmt.Errorf("search donor offers: %w", err)
	}

	// Index donor offers by data center, sorted by cost
	dcDonorOffers := make(map[string][]cloud.Offer)
	for _, o := range donorOffers {
		if o.DataCenter != "" {
			dcDonorOffers[o.DataCenter] = append(dcDonorOffers[o.DataCenter], o)
		}
	}
	for dc := range dcDonorOffers {
		sort.Slice(dcDonorOffers[dc], func(i, j int) bool {
			return dcDonorOffers[dc][i].CostPerHour < dcDonorOffers[dc][j].CostPerHour
		})
	}

	// Evaluate each DC: find the one where a donor saves the most
	var bestDC string
	var bestDonorOffer cloud.Offer
	var bestSavings float64

	for dc, workers := range dcWorkers {
		if len(workers) < 2 {
			continue // need 2+ workers in the same DC for a donor to help
		}
		collocated := dcDonorOffers[dc]
		if len(collocated) == 0 {
			slog.Debug("no collocated offers in data center", "component", "donor", "data_center", dc)
			continue
		}
		cheapest := collocated[0]
		savings := donorSavings(cheapest, workers)
		slog.Debug("donor savings analysis", "component", "donor", "data_center", dc, "workers", len(workers), "savings", savings, "donor_cost_per_hr", cheapest.CostPerHour)
		if savings > bestSavings {
			bestSavings = savings
			bestDC = dc
			bestDonorOffer = cheapest
		}
	}

	if bestDC == "" || bestSavings <= 0 {
		slog.Debug("donor not cost-effective in any data center", "component", "donor")
		return nil, nil
	}

	hfModels, hfDatasets := collectHFAssets(groups)
	var sourceDirs []string
	seen := make(map[string]bool)
	for _, g := range groups {
		for _, d := range g.SourceDirs() {
			if !seen[d] {
				seen[d] = true
				sourceDirs = append(sourceDirs, d)
			}
		}
	}

	return &DonorConfig{
		Offer:      bestDonorOffer,
		DataCenter: bestDC,
		HFModels:   hfModels,
		HFDatasets: hfDatasets,
		SourceDirs: sourceDirs,
	}, nil
}

// donorSavings calculates how much money a donor saves vs. independent downloads
// for workers in a single data center. Returns positive if donor is cheaper.
//
// WITHOUT donor (per worker): workerRate × downloadTime
// WITH donor (total):         donorRate × (downloadTime + copyTime) + sum(workerRate_i × copyTime)
//
// downloadTime = max across workers (they all download the same models)
// copyTime = estimated from cache bytes at LAN rate
func donorSavings(donorOffer cloud.Offer, workers []dcWorker) float64 {
	// Compute per-worker download time and total cost without donor
	var costWithout float64
	var maxDownloadTime time.Duration
	var maxCacheBytes int64

	for _, w := range workers {
		downloadTime := workerDownloadTime(w.estimate)
		costWithout += downloadTime.Hours() * w.offer.CostPerHour
		if downloadTime > maxDownloadTime {
			maxDownloadTime = downloadTime
		}
		cacheBytes := w.estimate.DownloadBytes + w.estimate.UVSyncBytes
		if cacheBytes > maxCacheBytes {
			maxCacheBytes = cacheBytes
		}
	}

	if costWithout == 0 {
		return 0
	}

	// Estimate LAN copy time from the largest cache
	copyTime := time.Duration(float64(maxCacheBytes) / estimatedLANCopyRate * float64(time.Second))

	// Donor cost: runs during download + copy phases
	donorCost := (maxDownloadTime + copyTime).Hours() * donorOffer.CostPerHour

	// Worker copy cost: each worker pays GPU rates during copy (instead of download)
	var workerCopyCost float64
	for _, w := range workers {
		workerCopyCost += copyTime.Hours() * w.offer.CostPerHour
	}

	costWith := donorCost + workerCopyCost

	slog.Debug("donor cost breakdown", "component", "donor", "cost_without", costWithout, "cost_with", costWith, "donor_cost", donorCost, "worker_copy_cost", workerCopyCost, "download_time", maxDownloadTime.Round(time.Second), "copy_time", copyTime.Round(time.Second))

	return costWithout - costWith
}

// workerDownloadTime estimates total download time for a worker from its cost estimate.
func workerDownloadTime(est CostEstimate) time.Duration {
	downloadTime := est.DownloadTime
	if est.UVSyncBytes > 0 && est.Offer.Offer != nil {
		bps := cloud.MbpsToBytesPerSec(est.Offer.Offer.DownloadBandwidth)
		if bps > 0 {
			downloadTime += time.Duration(float64(est.UVSyncBytes) / bps * float64(time.Second))
		}
	}
	return downloadTime
}

// collectHFAssets extracts HF model and dataset IDs from all jobs across all groups.
func collectHFAssets(groups []InstanceGroup) ([]string, []string) {
	seenModels := make(map[string]bool)
	seenDatasets := make(map[string]bool)
	var models []string
	var datasets []string
	for _, g := range groups {
		for _, ref := range g.AllInputs() {
			asset, ok := dataloc.ParseAssetRef(ref)
			if !ok {
				continue
			}
			switch asset.Kind {
			case dataloc.AssetHFModel:
				if !seenModels[asset.ID] {
					seenModels[asset.ID] = true
					models = append(models, asset.ID)
				}
			case dataloc.AssetHFDataset:
				if !seenDatasets[asset.ID] {
					seenDatasets[asset.ID] = true
					datasets = append(datasets, asset.ID)
				}
			}
		}
	}
	return models, datasets
}

// collectHFModels extracts HF model IDs from all jobs across all groups.
func collectHFModels(groups []InstanceGroup) []string {
	models, _ := collectHFAssets(groups)
	return models
}

// collectHFDatasets extracts HF dataset IDs from all jobs across all groups.
func collectHFDatasets(groups []InstanceGroup) []string {
	_, datasets := collectHFAssets(groups)
	return datasets
}

// workerInfo holds the provider and DB IDs of a worker instance.
type workerInfo struct {
	ProviderID string
	DBID       int64
}

// SeedWorkers copies caches from the donor to workers using a phone-tree fan-out.
// Each completed worker becomes a new donor, giving O(log N) total copy time.
func SeedWorkers(
	client cloud.Client,
	database *sql.DB,
	donorProviderID string,
	workers []workerInfo,
	cachePaths []string,
	onProgress func(workerDBID int64, phase string),
) error {
	if len(workers) == 0 || len(cachePaths) == 0 {
		return nil
	}
	if onProgress == nil {
		onProgress = func(int64, string) {}
	}

	// Channel for available donors (initially just the original donor)
	donors := make(chan string, len(workers)+1)
	donors <- donorProviderID

	var mu sync.Mutex
	remaining := make([]workerInfo, len(workers))
	copy(remaining, workers)

	var wg sync.WaitGroup
	var errs []error

	for {
		mu.Lock()
		if len(remaining) == 0 {
			mu.Unlock()
			break
		}
		mu.Unlock()

		// Wait for a donor to become available
		var donorID string
		select {
		case donorID = <-donors:
		default:
			// All donors are busy; wait for one
			donorID = <-donors
		}

		// Pop a worker
		mu.Lock()
		if len(remaining) == 0 {
			donors <- donorID
			mu.Unlock()
			break
		}
		worker := remaining[0]
		remaining = remaining[1:]
		mu.Unlock()

		wg.Add(1)
		go func(donor string, w workerInfo) {
			defer wg.Done()

			onProgress(w.DBID, "copying caches")
			start := time.Now()
			var copyErr error

			for _, cachePath := range cachePaths {
				copyErr = retry.Do(context.Background(), retry.FixedAttempts(3, 0), func() error {
					return client.CopyBetweenInstances(donor, cachePath, w.ProviderID, cachePath)
				}, retry.WithOnRetry(func(attempt int, err error, delay time.Duration) {
					slog.Warn("donor copy to worker failed", "component", "donor", "cache_path", cachePath, "worker", w.ProviderID, "attempt", attempt, "error", err)
				}))
				if copyErr != nil {
					break
				}
			}

			elapsed := int(time.Since(start).Seconds())
			if copyErr != nil {
				slog.Warn("donor copy to worker failed after retries", "component", "donor", "worker", w.ProviderID, "error", copyErr)
				mu.Lock()
				errs = append(errs, fmt.Errorf("worker %d: %w", w.DBID, copyErr))
				mu.Unlock()
				onProgress(w.DBID, "copy failed, will download independently")
			} else {
				_ = db.SetLaunchSeedCopySecs(database, w.DBID, elapsed)
				recordDonorTransferObservation(database, donor, w.ProviderID, w.DBID, time.Since(start))
				onProgress(w.DBID, fmt.Sprintf("copy complete (%ds)", elapsed))
				// This worker can now be a donor for others
				donors <- w.ProviderID
			}
		}(donorID, worker)
	}

	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("some worker copies failed: %d/%d workers", len(errs), len(workers))
	}
	return nil
}

func recordDonorTransferObservation(database *sql.DB, donorProviderID, workerProviderID string, workerDBID int64, elapsed time.Duration) {
	if database == nil || elapsed <= 0 {
		return
	}
	worker, err := db.GetLaunch(database, workerDBID)
	if err != nil || worker == nil {
		return
	}
	bytesTransferred := estimateProvisionedInputBytes(worker.ProvisionedInputs)
	if bytesTransferred <= 0 {
		return
	}
	source := transferbw.DonorEndpoint(donorProviderID)
	dest := transferbw.CloudEndpoint(worker.Provider, worker.DataCenter, workerProviderID)
	if err := transferbw.RecordObservation(database, source, dest, bytesTransferred, elapsed); err != nil {
		slog.Warn("failed to record donor transfer observation", "component", "donor", "worker", workerProviderID, "error", err)
	}
}

func estimateProvisionedInputBytes(inputs []string) int64 {
	if len(inputs) == 0 {
		return 0
	}
	bytes, _, err := dataloc.ResolveInputSizes(inputs, nil)
	if err != nil {
		slog.Warn("failed to resolve donor transfer input sizes", "component", "donor", "error", err)
	}
	return bytes
}

// DefaultDonorReadyTimeout is how long to wait for donor to finish downloads.
const DefaultDonorReadyTimeout = 30 * time.Minute

// DefaultDonorCachePaths are the cache directories copied from donor to workers.
var DefaultDonorCachePaths = []string{
	"/root/.cache/uv",
	"/root/.cache/huggingface",
}
