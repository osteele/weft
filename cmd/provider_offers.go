package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/runpod"
	"github.com/osteele/weft/internal/vastai"
	"github.com/spf13/cobra"
)

var (
	providerOffersProvider        string
	providerOffersRunpodCloudType string
	providerOffersMinGPUMemGB     int
	providerOffersMinDiskGB       int
	providerOffersMinReliability  float64
	providerOffersMinSurvival     float64
	providerOffersNumGPUs         int
	providerOffersNeed            int
	providerOffersInstanceType    string
	providerOffersMinDriver       int
	providerOffersMinCUDA         string
	providerOffersJSON            bool
)

var providerOffersCmd = &cobra.Command{
	Use:   "offers [GPU...]",
	Short: "Report live provider offers with survival estimates",
	Long: `Report live provider offers together with Weft's survival estimate.

The report is read-only. It searches provider offers with the supplied resource
constraints, groups matching offers by provider and GPU type, and shows whether
enough candidates clear the requested survival floor.

RunPod exposes GPU-type stock labels, not physical machine counts, so RunPod
rows can be "unknown" for multi-machine needs even when stock is present.`,
	Args: cobra.ArbitraryArgs,
	RunE: runProviderOffers,
}

var defaultProviderOfferGPUClasses = []string{
	"rtx_3090",
	"rtx_4090",
	"l4",
	"l40",
	"l40s",
	"a40",
	"rtx_a6000",
	"h100",
	"h200",
}

type providerOfferReportRow struct {
	Provider         string `json:"provider"`
	Request          string `json:"request"`
	GPU              string `json:"gpu"`
	VRAM             string `json:"vram"`
	Stock            string `json:"stock,omitempty"`
	LiveOffers       int    `json:"live_offers"`
	ViableOffers     int    `json:"viable_offers"`
	DistinctMachines *int   `json:"distinct_machines,omitempty"`
	Survival         string `json:"survival"`
	Price            string `json:"price"`
	Enough           string `json:"enough"`
	Status           string `json:"status"`
	Error            string `json:"error,omitempty"`
}

type providerOfferSearchResult struct {
	request  string
	provider string
	offers   []cloud.Offer
	err      error
}

type providerOfferSurvivalEstimator func(cloud.Offer) (float64, bool)

func init() {
	providerCmd.AddCommand(providerOffersCmd)
	providerOffersCmd.Flags().StringVar(&providerOffersProvider, "provider", "", "Restrict report to one provider (vastai or runpod)")
	providerOffersCmd.Flags().StringVar(&providerOffersRunpodCloudType, "runpod-cloud-type", "", "RunPod cloud type to search: community or secure (default from config)")
	providerOffersCmd.Flags().IntVar(&providerOffersMinGPUMemGB, "min-gpu-mem", 0, "Minimum per-GPU memory in GB")
	providerOffersCmd.Flags().IntVar(&providerOffersMinDiskGB, "min-disk", 0, "Minimum disk space in GB")
	providerOffersCmd.Flags().Float64Var(&providerOffersMinReliability, "reliability", cloud.DefaultMinReliability, "Minimum provider reliability score (0-1)")
	providerOffersCmd.Flags().Float64Var(&providerOffersMinSurvival, "min-survival", 0.4, "Minimum Weft learned end-to-end survival probability (0-1; distinct from provider reliability)")
	providerOffersCmd.Flags().IntVar(&providerOffersNumGPUs, "num-gpus", 1, "Number of GPUs per offer")
	providerOffersCmd.Flags().IntVar(&providerOffersNeed, "need", 1, "Number of distinct machines/jobs the GPU type must satisfy")
	providerOffersCmd.Flags().StringVar(&providerOffersInstanceType, "instance-type", "", "Instance type to search: on-demand or interruptible")
	providerOffersCmd.Flags().IntVar(&providerOffersMinDriver, "driver-min", 0, "Minimum NVIDIA driver major version")
	providerOffersCmd.Flags().StringVar(&providerOffersMinCUDA, "cuda-driver-min", "", "Minimum NVIDIA driver CUDA support (for example 12.8)")
	providerOffersCmd.Flags().BoolVar(&providerOffersJSON, "json", false, "Print report as JSON")
}

func runProviderOffers(cmd *cobra.Command, args []string) error {
	if providerOffersMinReliability < 0 || providerOffersMinReliability > 1 {
		return fmt.Errorf("--reliability must be between 0 and 1")
	}
	if providerOffersMinSurvival < 0 || providerOffersMinSurvival > 1 {
		return fmt.Errorf("--min-survival must be between 0 and 1")
	}
	if providerOffersNeed < 1 {
		return fmt.Errorf("--need must be at least 1")
	}
	if providerOffersNumGPUs < 1 {
		return fmt.Errorf("--num-gpus must be at least 1")
	}
	instanceType, err := normalizeProviderOffersInstanceType(providerOffersInstanceType)
	if err != nil {
		return err
	}
	runpodCloudType, err := normalizeRunpodCloudTypeFlag(providerOffersRunpodCloudType)
	if err != nil {
		return err
	}
	provider, err := normalizeProviderFlag(providerOffersProvider)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	clients, err := providerOfferClients(cfg, provider, runpodCloudType)
	if err != nil {
		return err
	}
	if len(clients) == 0 {
		return fmt.Errorf("no cloud providers configured")
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	model := buildProviderOffersSurvivalModel(database)
	estimator := func(offer cloud.Offer) (float64, bool) {
		if model == nil {
			return 0, false
		}
		return model.OfferSurvival(offer), true
	}

	gpus := args
	if len(gpus) == 0 {
		gpus = defaultProviderOfferGPUClasses
	}

	results := make([]providerOfferSearchResult, 0, len(gpus))
	resultProvider := providerOffersResultProvider(provider, clients)
	for _, gpu := range gpus {
		constraints := cloud.OfferConstraints{
			GPUClass:         gpu,
			MinGPUMemGB:      providerOffersMinGPUMemGB,
			MinDiskGB:        providerOffersMinDiskGB,
			MinReliability:   providerOffersMinReliability,
			MinDriverVersion: providerOffersMinDriver,
			MinCUDAVersion:   providerOffersMinCUDA,
			NumGPUs:          providerOffersNumGPUs,
			InstanceType:     instanceType,
			RunpodCloudType:  runpodCloudType,
		}
		offers, err := searchProviderOffers(clients, constraints)
		results = append(results, providerOfferSearchResult{
			request:  gpu,
			provider: resultProvider,
			offers:   offers,
			err:      err,
		})
	}

	rows := providerOfferRows(results, providerOffersMinSurvival, providerOffersNeed, estimator)
	annotateProviderOfferRows(rows, providerOffersMinDriver > 0 || strings.TrimSpace(providerOffersMinCUDA) != "")
	sortProviderOfferRows(rows)
	if providerOffersJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	printProviderOfferRows(cmd, rows)
	return nil
}

func buildProviderOffersSurvivalModel(database *sql.DB) *bidding.SurvivalModel {
	if database == nil {
		return nil
	}
	return buildSurvivalModel(database)
}

func normalizeProviderOffersInstanceType(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "":
		return "", nil
	case cloud.InstanceTypeOnDemand, "ondemand":
		return cloud.InstanceTypeOnDemand, nil
	case cloud.InstanceTypeInterruptible, "interrupt", "preemptible", "spot":
		return cloud.InstanceTypeInterruptible, nil
	default:
		return "", fmt.Errorf("unknown --instance-type %q (expected on-demand or interruptible)", raw)
	}
}

func providerOfferClients(cfg *config.Config, provider, runpodCloudType string) ([]cloud.Client, error) {
	if provider == "" {
		return buildCloudClients(cfg)
	}
	switch cloud.Provider(provider) {
	case cloud.ProviderVastai:
		return []cloud.Client{vastai.NewCloudClient(vastai.NewClient())}, nil
	case cloud.ProviderRunpod:
		cloudType := runpodCloudType
		if cloudType == "" && cfg != nil {
			var err error
			cloudType, err = cfg.RunpodCloudType()
			if err != nil {
				return nil, err
			}
		}
		if cloudType == "" {
			cloudType = cloud.DefaultRunpodCloudType
		}
		return []cloud.Client{runpod.NewCloudClientWithCloudType(cloudType)}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q", provider)
	}
}

func searchProviderOffers(clients []cloud.Client, constraints cloud.OfferConstraints) ([]cloud.Offer, error) {
	if len(clients) == 1 {
		return clients[0].SearchOffers(constraints)
	}
	return cloud.SearchAllProviders(clients, constraints)
}

func providerOffersResultProvider(provider string, clients []cloud.Client) string {
	if provider != "" {
		return provider
	}
	if len(clients) == 1 {
		return string(clients[0].Provider())
	}
	return ""
}

func providerOfferRows(results []providerOfferSearchResult, floor float64, need int, estimator providerOfferSurvivalEstimator) []providerOfferReportRow {
	rows := make([]providerOfferReportRow, 0, len(results))
	for _, result := range results {
		if result.err != nil {
			rows = append(rows, providerOfferReportRow{
				Provider: result.provider,
				Request:  result.request,
				GPU:      result.request,
				VRAM:     "?",
				Enough:   "no",
				Survival: "?",
				Price:    "?",
				Status:   "search failed",
				Error:    result.err.Error(),
			})
			continue
		}
		if len(result.offers) == 0 {
			rows = append(rows, providerOfferReportRow{
				Provider: result.provider,
				Request:  result.request,
				GPU:      result.request,
				VRAM:     "?",
				Enough:   "no",
				Survival: "?",
				Price:    "?",
				Status:   "no live offers",
			})
			continue
		}
		rows = append(rows, summarizeProviderOfferGroups(result.request, result.offers, floor, need, estimator)...)
	}
	return rows
}

func annotateProviderOfferRows(rows []providerOfferReportRow, requiresDriverCUDA bool) {
	if !requiresDriverCUDA {
		return
	}
	for i := range rows {
		if rows[i].Provider != string(cloud.ProviderRunpod) || rows[i].LiveOffers == 0 {
			continue
		}
		if rows[i].Enough == "yes" {
			rows[i].Enough = "unknown"
		}
		rows[i].Status = appendStatus(rows[i].Status, "driver/CUDA checked after launch")
	}
}

func appendStatus(base, extra string) string {
	if strings.TrimSpace(base) == "" {
		return extra
	}
	return base + "; " + extra
}

func summarizeProviderOfferGroups(request string, offers []cloud.Offer, floor float64, need int, estimator providerOfferSurvivalEstimator) []providerOfferReportRow {
	type offerWithSurvival struct {
		offer       cloud.Offer
		survival    float64
		hasSurvival bool
	}
	groups := make(map[string][]offerWithSurvival)
	for _, offer := range offers {
		survival, ok := estimator(offer)
		key := strings.Join([]string{
			string(offer.Provider),
			displayOfferGPU(offer, request),
			formatVRAM(offer.GPUMemGB),
			stockLabel(offer.StockStatus),
		}, "\x00")
		groups[key] = append(groups[key], offerWithSurvival{offer: offer, survival: survival, hasSurvival: ok})
	}

	rows := make([]providerOfferReportRow, 0, len(groups))
	for _, group := range groups {
		first := group[0].offer
		var survivals []float64
		var viablePrices []float64
		var fallbackPrices []float64
		viable := 0
		knownMachines := map[string]struct{}{}
		anyMachineID := false
		anySurvival := false
		for _, item := range group {
			if item.offer.CostPerHour > 0 {
				fallbackPrices = append(fallbackPrices, item.offer.CostPerHour)
			}
			if item.offer.MachineID != "" {
				anyMachineID = true
			}
			if item.hasSurvival {
				anySurvival = true
				survivals = append(survivals, item.survival)
			}
			if item.hasSurvival && item.survival >= floor {
				viable++
				if item.offer.CostPerHour > 0 {
					viablePrices = append(viablePrices, item.offer.CostPerHour)
				}
				if item.offer.MachineID != "" {
					knownMachines[item.offer.MachineID] = struct{}{}
				}
			}
		}

		var distinct *int
		if anyMachineID {
			count := len(knownMachines)
			distinct = &count
		}
		enough, status := providerOfferEnoughStatus(first.Provider, stockLabel(first.StockStatus), len(group), viable, distinct, anySurvival, need)
		prices := viablePrices
		if len(prices) == 0 {
			prices = fallbackPrices
		}
		rows = append(rows, providerOfferReportRow{
			Provider:         string(first.Provider),
			Request:          request,
			GPU:              displayOfferGPU(first, request),
			VRAM:             formatVRAM(first.GPUMemGB),
			Stock:            stockLabel(first.StockStatus),
			LiveOffers:       len(group),
			ViableOffers:     viable,
			DistinctMachines: distinct,
			Survival:         formatProbabilityRange(survivals),
			Price:            formatPriceSummary(prices),
			Enough:           enough,
			Status:           status,
		})
	}
	return rows
}

func providerOfferEnoughStatus(provider cloud.Provider, stock string, live, viable int, distinct *int, anySurvival bool, need int) (string, string) {
	if live == 0 {
		return "no", "no live offers"
	}
	if !anySurvival {
		return "unknown", "no survival data"
	}
	if viable == 0 {
		return "no", "below survival floor"
	}
	if distinct != nil {
		if *distinct >= need {
			return "yes", "ok"
		}
		return "no", fmt.Sprintf("only %d distinct machine(s)", *distinct)
	}
	if provider == cloud.ProviderRunpod && need > 1 {
		if stock != "" {
			return "unknown", "RunPod exposes stock label, not machine count"
		}
		return "unknown", "RunPod machine count unknown"
	}
	if viable >= need {
		return "unknown", "distinct machine count unknown"
	}
	return "no", fmt.Sprintf("only %d viable offer(s)", viable)
}

func displayOfferGPU(offer cloud.Offer, fallback string) string {
	name := strings.TrimSpace(offer.GPUName)
	if name == "" {
		return fallback
	}
	return name
}

func stockLabel(stock string) string {
	return strings.TrimSpace(stock)
}

func formatVRAM(v float64) string {
	if v <= 0 {
		return "?"
	}
	if math.Abs(v-math.Round(v)) < 0.05 {
		return fmt.Sprintf("%.0fGB", v)
	}
	return fmt.Sprintf("%.1fGB", v)
}

func formatProbabilityRange(values []float64) string {
	if len(values) == 0 {
		return "?"
	}
	sort.Float64s(values)
	minVal := values[0]
	maxVal := values[len(values)-1]
	if math.Abs(minVal-maxVal) < 0.005 {
		return fmt.Sprintf("%.0f%%", minVal*100)
	}
	return fmt.Sprintf("%.0f-%.0f%%", minVal*100, maxVal*100)
}

func formatPriceSummary(values []float64) string {
	if len(values) == 0 {
		return "?"
	}
	sort.Float64s(values)
	minVal := values[0]
	median := medianFloat(values)
	if math.Abs(minVal-median) < 0.0005 {
		return fmt.Sprintf("$%.3f/hr", minVal)
	}
	return fmt.Sprintf("$%.3f/$%.3f/hr", minVal, median)
}

func medianFloat(sorted []float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func sortProviderOfferRows(rows []providerOfferReportRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		if rows[i].Request != rows[j].Request {
			return rows[i].Request < rows[j].Request
		}
		if rows[i].GPU != rows[j].GPU {
			return rows[i].GPU < rows[j].GPU
		}
		return rows[i].VRAM < rows[j].VRAM
	})
}

func printProviderOfferRows(cmd *cobra.Command, rows []providerOfferReportRow) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tREQUEST\tGPU\tVRAM\tSTOCK\tLIVE\tVIABLE\tMACHINES\tSURVIVAL\tPRICE\tENOUGH\tSTATUS")
	for _, row := range rows {
		machines := "?"
		if row.DistinctMachines != nil {
			machines = fmt.Sprintf("%d", *row.DistinctMachines)
		}
		provider := row.Provider
		if provider == "" {
			provider = "-"
		}
		status := row.Status
		if row.Error != "" {
			status += ": " + row.Error
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			provider, row.Request, row.GPU, row.VRAM, row.Stock,
			row.LiveOffers, row.ViableOffers, machines, row.Survival,
			row.Price, row.Enough, status)
	}
	w.Flush()
}
