package cmd

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/spf13/cobra"
)

var (
	priceSpreadReliability float64
	priceSpreadMinMemGB    int
	priceSpreadNumGPUs     int
	priceSpreadJSON        bool
)

var cloudPriceSpreadCmd = &cobra.Command{
	Use:   "price-spread [GPU...]",
	Short: "Compare on-demand vs interruptible offer prices per GPU class",
	Long: `Compare on-demand vs interruptible offer prices per GPU class across
configured cloud providers.

For each GPU class, queries the provider for both on-demand and interruptible
offers (all providers in parallel) and reports the minimum and median $/hr in
each market, plus the corresponding savings percentages.

Interruptible savings are driven by the bid market: popular GPUs (4090, A100,
H100) tend to clear near the on-demand ask; less-loved machines (older 3090s,
niche regions) often show a wider gap.

Default GPUs: rtx_3090 rtx_4090 a100 h100`,
	Args: cobra.ArbitraryArgs,
	RunE: runCloudPriceSpread,
}

func init() {
	cloudCmd.AddCommand(cloudPriceSpreadCmd)
	cloudPriceSpreadCmd.Flags().Float64Var(&priceSpreadReliability, "reliability", cloud.DefaultMinReliability, "minimum offer reliability (0-1)")
	cloudPriceSpreadCmd.Flags().IntVar(&priceSpreadMinMemGB, "min-gpu-mem", 0, "minimum per-GPU memory in GB")
	cloudPriceSpreadCmd.Flags().IntVar(&priceSpreadNumGPUs, "num-gpus", 1, "number of GPUs per offer")
	cloudPriceSpreadCmd.Flags().BoolVar(&priceSpreadJSON, "json", false, "output as JSON")
}

type priceSpreadRow struct {
	GPU             string  `json:"gpu"`
	OnDemandMin     float64 `json:"on_demand_min"`
	OnDemandMedian  float64 `json:"on_demand_median"`
	OnDemandCount   int     `json:"on_demand_count"`
	InterruptMin    float64 `json:"interruptible_min"`
	InterruptMedian float64 `json:"interruptible_median"`
	InterruptCount  int     `json:"interruptible_count"`
	// SavingsMin / SavingsMedian: fraction in [0,1], NaN if undefined.
	SavingsMin    float64 `json:"savings_min"`
	SavingsMedian float64 `json:"savings_median"`
	Provider      string  `json:"provider_min"` // provider of the cheapest interruptible offer
}

func runCloudPriceSpread(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	clients, err := buildCloudClients(cfg)
	if err != nil {
		return err
	}
	if len(clients) == 0 {
		return fmt.Errorf("no cloud providers configured")
	}

	gpus := args
	if len(gpus) == 0 {
		gpus = []string{"rtx_3090", "rtx_4090", "a100", "h100"}
	}

	rows := make([]priceSpreadRow, len(gpus))
	var wg sync.WaitGroup
	for i, gpu := range gpus {
		wg.Add(1)
		go func(idx int, gpuClass string) {
			defer wg.Done()
			rows[idx] = computePriceSpread(clients, gpuClass)
		}(i, gpu)
	}
	wg.Wait()

	if priceSpreadJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	printPriceSpreadTable(rows)
	return nil
}

func computePriceSpread(clients []cloud.Client, gpuClass string) priceSpreadRow {
	row := priceSpreadRow{GPU: gpuClass}
	base := cloud.OfferConstraints{
		GPUClass:       gpuClass,
		MinGPUMemGB:    priceSpreadMinMemGB,
		MinReliability: priceSpreadReliability,
		NumGPUs:        priceSpreadNumGPUs,
	}

	onDemand := base
	onDemand.InstanceType = cloud.InstanceTypeOnDemand
	interrupt := base
	interrupt.InstanceType = cloud.InstanceTypeInterruptible

	odOffers, _ := cloud.SearchAllProviders(clients, onDemand)
	intOffers, _ := cloud.SearchAllProviders(clients, interrupt)

	odCosts := extractCosts(odOffers)
	intCosts := extractCosts(intOffers)

	row.OnDemandCount = len(odCosts)
	row.InterruptCount = len(intCosts)
	row.OnDemandMin = minOrNaN(odCosts)
	row.OnDemandMedian = medianOrNaN(odCosts)
	row.InterruptMin = minOrNaN(intCosts)
	row.InterruptMedian = medianOrNaN(intCosts)
	row.SavingsMin = savingsRatio(row.OnDemandMin, row.InterruptMin)
	row.SavingsMedian = savingsRatio(row.OnDemandMedian, row.InterruptMedian)
	row.Provider = cheapestProvider(intOffers)

	return row
}

func extractCosts(offers []cloud.Offer) []float64 {
	costs := make([]float64, 0, len(offers))
	for _, o := range offers {
		if o.CostPerHour > 0 {
			costs = append(costs, o.CostPerHour)
		}
	}
	sort.Float64s(costs)
	return costs
}

func minOrNaN(sorted []float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	return sorted[0]
}

func medianOrNaN(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return math.NaN()
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func savingsRatio(ask, bid float64) float64 {
	if math.IsNaN(ask) || math.IsNaN(bid) || ask <= 0 {
		return math.NaN()
	}
	return (ask - bid) / ask
}

func cheapestProvider(offers []cloud.Offer) string {
	var best *cloud.Offer
	for i := range offers {
		o := &offers[i]
		if o.CostPerHour <= 0 {
			continue
		}
		if best == nil || o.CostPerHour < best.CostPerHour {
			best = o
		}
	}
	if best == nil {
		return ""
	}
	return string(best.Provider)
}

func printPriceSpreadTable(rows []priceSpreadRow) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "GPU\tON-DEMAND min/med ($/hr)\tINTERRUPTIBLE min/med ($/hr)\tSAVINGS min/med\tOFFERS od/int")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d/%d\n",
			r.GPU,
			formatPricePair(r.OnDemandMin, r.OnDemandMedian),
			formatPricePair(r.InterruptMin, r.InterruptMedian),
			formatSavingsPair(r.SavingsMin, r.SavingsMedian),
			r.OnDemandCount, r.InterruptCount,
		)
	}
	w.Flush()

	anyMissing := false
	for _, r := range rows {
		if r.OnDemandCount == 0 || r.InterruptCount == 0 {
			anyMissing = true
			break
		}
	}
	if anyMissing {
		fmt.Println()
		fmt.Println("Note: rows with 0 offers indicate no matching offers at the configured reliability floor")
		fmt.Println("      or no provider adapter that supports interruptible for that GPU class.")
	}
}

func formatPricePair(minVal, median float64) string {
	return formatPrice(minVal) + " / " + formatPrice(median)
}

func formatPrice(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("$%.3f", v)
}

func formatSavingsPair(minVal, median float64) string {
	s := formatSavings(minVal) + " / " + formatSavings(median)
	return strings.TrimSpace(s)
}

func formatSavings(v float64) string {
	if math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", v*100)
}
