package campaign

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/compat"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/predictor"
)

// parseCUDAVersionFloat converts a CUDA major.minor string (e.g. "12.4") to
// float64 for comparison with placement.MinCUDAForGPU values.
func parseCUDAVersionFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// OfferFilterStats tracks how many offers survived each filtering stage.
// Fields are evaluated in order: VRAM → GPU count → host RAM → interconnect →
// CUDA → TorchArch → Survival. A zero value means either "all filtered out at
// this stage" or "stage not reached" (because an earlier stage already
// eliminated everything).
type OfferFilterStats struct {
	RawCount          int // offers returned by provider search
	AfterVRAM         int // remaining after VRAM requirement filter
	AfterGPUCount     int // remaining after GPU count requirement filter
	AfterHostRAM      int // remaining after host/system RAM requirement filter
	AfterInterconnect int // remaining after interconnect topology requirement filter
	AfterCUDA         int // remaining after CUDA compatibility filter
	AfterProvider     int // remaining after provider CUDA/driver compatibility filter
	AfterForward      int // remaining after Vast.ai forward-compat driver guard
	AfterTorchArch    int // remaining after torch arch compute-cap filter
	AfterSurvival     int // remaining after survival probability filter
	// UnknownCompatibility tracks offers whose provider did not report
	// CUDA/driver compatibility even though the group requires a floor.
	UnknownCompatibility          int
	GPUCountFiltered              int
	HostRAMFiltered               int
	InterconnectFiltered          int
	InterconnectRequired          string
	ProviderCompatibilityFiltered int
	ForwardCompatFiltered         int
	ForwardCompatExampleGPU       string
	// CUDA filter diagnostics (set when CUDA filtering removed offers).
	CUDAImage        string
	CUDAImageVersion float64
	CUDAMinRequired  float64
	CUDAExampleGPU   string
	// Torch arch filter diagnostics (set when arch filtering removed offers).
	TorchArchMinCap     string
	TorchArchMaxCap     string
	TorchArchExampleCap string
	TorchArchExampleGPU string
	// ProviderErrors records providers whose search failed while another
	// provider returned offers. These errors are important context when the
	// surviving offers are later filtered out.
	ProviderErrors []string
}

// NoOffersDetail returns a human-readable explanation of why no offers survived
// filtering. When constraints is non-empty it is appended to the zero-offers
// branch so the caller can see which search predicates yielded nothing.
func (s OfferFilterStats) NoOffersDetail(constraints string) string {
	if s.RawCount == 0 {
		if len(s.ProviderErrors) > 0 {
			detail := strings.Join(s.ProviderErrors, "; ")
			if detail != "" {
				return "provider offer fetch unavailable: " + detail + "; market unknown; Weft will retry"
			}
			return "provider offer fetch unavailable (market unknown; Weft will retry)"
		}
		if constraints != "" {
			return fmt.Sprintf("no offers from providers for %s", constraints)
		}
		return "no offers from providers"
	}
	found := offerCount(s.RawCount) + " found"
	passedGPUCount := s.AfterGPUCount
	if passedGPUCount == 0 {
		passedGPUCount = s.AfterVRAM
	}
	passedHostRAM := s.AfterHostRAM
	if passedHostRAM == 0 {
		passedHostRAM = passedGPUCount
	}
	passedInterconnect := s.AfterInterconnect
	if passedInterconnect == 0 {
		passedInterconnect = passedHostRAM
	}
	passedProvider := s.AfterProvider
	if passedProvider == 0 {
		passedProvider = s.AfterCUDA
	}
	passedForward := s.AfterForward
	if passedForward == 0 {
		passedForward = passedProvider
	}
	switch {
	case s.AfterVRAM == 0:
		return fmt.Sprintf("%s, all filtered by VRAM requirement", found)
	case s.GPUCountFiltered > 0 && s.AfterGPUCount == 0:
		return fmt.Sprintf("%s, %s passed VRAM but all filtered by GPU count requirement", found, offerCount(s.AfterVRAM))
	case s.HostRAMFiltered > 0 && s.AfterHostRAM == 0:
		return fmt.Sprintf("%s, %s passed VRAM/GPU count but all filtered by host RAM requirement", found, offerCount(passedGPUCount))
	case s.InterconnectFiltered > 0 && s.AfterInterconnect == 0:
		if s.InterconnectRequired != "" {
			return fmt.Sprintf("%s, %s passed host RAM but all filtered by interconnect requirement (%s)", found, offerCount(passedHostRAM), s.InterconnectRequired)
		}
		return fmt.Sprintf("%s, %s passed host RAM but all filtered by interconnect requirement", found, offerCount(passedHostRAM))
	case s.AfterCUDA == 0:
		if s.CUDAMinRequired > 0 && s.CUDAImageVersion > 0 {
			if s.CUDAExampleGPU != "" && s.CUDAImage != "" {
				return fmt.Sprintf("%s, %s passed interconnect/topology filters but all filtered by CUDA compatibility (image=%s CUDA %.1f; requires >=%.1f, e.g. %s)", found, offerCount(passedInterconnect), s.CUDAImage, s.CUDAImageVersion, s.CUDAMinRequired, s.CUDAExampleGPU)
			}
			return fmt.Sprintf("%s, %s passed interconnect/topology filters but all filtered by CUDA compatibility (image CUDA %.1f; requires >=%.1f)", found, offerCount(passedInterconnect), s.CUDAImageVersion, s.CUDAMinRequired)
		}
		return fmt.Sprintf("%s, %s passed interconnect/topology filters but all filtered by CUDA compatibility", found, offerCount(passedInterconnect))
	case s.ProviderCompatibilityFiltered > 0 && s.AfterProvider == 0:
		if s.UnknownCompatibility > 0 {
			detail := fmt.Sprintf("%s, %s passed CUDA/image filters but all filtered because the provider did not report required CUDA/driver compatibility", found, offerCount(s.AfterCUDA))
			if len(s.ProviderErrors) > 0 {
				detail += "; known-compatible provider search also had errors: " + strings.Join(s.ProviderErrors, "; ")
			}
			return detail
		}
		return fmt.Sprintf("%s, %s passed CUDA/image filters but all filtered by provider CUDA/driver compatibility", found, offerCount(s.AfterCUDA))
	case s.ForwardCompatFiltered > 0 && s.AfterForward == 0:
		if s.ForwardCompatExampleGPU != "" {
			return fmt.Sprintf("%s, %s passed provider compatibility but all filtered by Vast.ai datacenter forward-compat driver guard on consumer GPUs (e.g. %s)", found, offerCount(passedProvider), s.ForwardCompatExampleGPU)
		}
		return fmt.Sprintf("%s, %s passed provider compatibility but all filtered by Vast.ai datacenter forward-compat driver guard on consumer GPUs", found, offerCount(passedProvider))
	case (s.TorchArchMinCap != "" || s.TorchArchMaxCap != "") && s.AfterTorchArch == 0:
		bounds := formatTorchArchBounds(s.TorchArchMinCap, s.TorchArchMaxCap)
		if s.TorchArchExampleGPU != "" && s.TorchArchExampleCap != "" {
			return fmt.Sprintf("%s, %s passed VRAM/CUDA but all filtered by torch arch %s (e.g. %s sm_%s)", found, offerCount(passedForward), bounds, s.TorchArchExampleGPU, s.TorchArchExampleCap)
		}
		return fmt.Sprintf("%s, %s passed VRAM/CUDA but all filtered by torch arch %s", found, offerCount(passedForward), bounds)
	case s.AfterSurvival == 0:
		passed := passedForward
		if s.TorchArchMinCap != "" || s.TorchArchMaxCap != "" {
			passed = s.AfterTorchArch
		}
		return fmt.Sprintf("%s, %s passed filters but none met survival threshold", found, offerCount(passed))
	default:
		// Defensive: all stages passed but no offer was selected.
		return fmt.Sprintf("%s, none met all criteria", found)
	}
}

func formatTorchArchBounds(minCap, maxCap string) string {
	switch {
	case minCap != "" && maxCap != "":
		return fmt.Sprintf("bounds (min cap=%s max cap=%s)", minCap, maxCap)
	case minCap != "":
		return fmt.Sprintf("lower bound (min cap=%s)", minCap)
	case maxCap != "":
		return fmt.Sprintf("upper bound (max cap=%s)", maxCap)
	default:
		return "bounds"
	}
}

func offerCount(n int) string {
	if n == 1 {
		return "1 offer"
	}
	return fmt.Sprintf("%d offers", n)
}

// FormatOfferConstraints renders an OfferConstraints as a compact human string
// like "gpu=ampere+ vram>=40GB disk>=60GB reliability>=0.95". Empty fields are
// omitted.
func FormatOfferConstraints(c cloud.OfferConstraints) string {
	var parts []string
	if c.GPUClass != "" {
		parts = append(parts, "gpu="+c.GPUClass)
	}
	if c.MinGPUMemGB > 0 {
		parts = append(parts, fmt.Sprintf("vram>=%dGB", c.MinGPUMemGB))
	}
	if c.MinDiskGB > 0 {
		parts = append(parts, fmt.Sprintf("disk>=%dGB", c.MinDiskGB))
	}
	if c.MinReliability > 0 {
		parts = append(parts, fmt.Sprintf("reliability>=%.2f", c.MinReliability))
	}
	if c.MinCPUCoresEffective > 0 {
		parts = append(parts, fmt.Sprintf("cpu>=%d", c.MinCPUCoresEffective))
	}
	if c.MinDriverVersion > 0 {
		parts = append(parts, fmt.Sprintf("driver>=%d", c.MinDriverVersion))
	}
	if c.MinCUDAVersion != "" {
		parts = append(parts, fmt.Sprintf("cuda>=%s", c.MinCUDAVersion))
	}
	return strings.Join(parts, " ")
}

// GroupOffer pairs an instance group with its best cloud offer.
type GroupOffer struct {
	Group          InstanceGroup
	Offer          *cloud.Offer // nil if no offers found
	SurvivalProb   float64      // 0 if no survival model available
	RejectedGroups []bidding.RejectedGroup
	FilterStats    OfferFilterStats
	Alternatives   []RankedOfferAlternative
	Err            error
}

// RankedOfferAlternative captures a scored cloud offer for retrospective
// placement analysis.
type RankedOfferAlternative struct {
	Rank          int
	Offer         cloud.Offer
	Score         float64
	CompletionHrs float64
	Cost          float64
	Survival      float64
	Compatibility CompatibilityStatus
	Selected      bool
}

// snapshotOnDemandRefCents returns the cheapest comparable on-demand $/hr (in
// cents) for the chosen offer, to record a counterfactual for preemptible
// savings analysis and a bounded bid-rescue ceiling. Comparable means the same
// normalized provider GPU name as the selected offer, not merely the broad
// placement class (e.g. "NVIDIA >=42GB"), because the broad class can contain
// cheaper, slower GPUs that are not a useful rescue ceiling.
//
// Best-effort: returns nil on any failure or when no comparable on-demand offer
// is visible.
// Intended for async use off the launch path.
func snapshotOnDemandRefCents(client cloud.Client, group InstanceGroup, chosen cloud.Offer) *int {
	constraints := offerConstraintsForGroup(group, cloud.DefaultMinReliability)
	constraints.InstanceType = cloud.InstanceTypeOnDemand
	constraints.NumGPUs = chosen.NumGPUs
	offers, err := client.SearchOffers(constraints)
	if err != nil {
		return nil
	}
	var cheapest float64
	for _, o := range offers {
		if o.CostPerHour <= 0 {
			continue
		}
		if !comparableOnDemandOffer(group, chosen, o) {
			continue
		}
		if cheapest == 0 || o.CostPerHour < cheapest {
			cheapest = o.CostPerHour
		}
	}
	if cheapest == 0 {
		return nil
	}
	cents := priceCentsCeil(cheapest)
	initialBidCents := priceCentsCeil(chosen.CostPerHour)
	if cents < initialBidCents {
		cents = initialBidCents
	}
	return &cents
}

func comparableOnDemandOffer(group InstanceGroup, chosen cloud.Offer, offer cloud.Offer) bool {
	if offer.InstanceType != "" && offer.InstanceType != cloud.InstanceTypeOnDemand {
		return false
	}
	if chosen.NumGPUs > 0 && offer.NumGPUs > 0 && offer.NumGPUs != chosen.NumGPUs {
		return false
	}
	if chosen.GPUMemGB > 0 && offer.GPUMemGB+0.01 < chosen.GPUMemGB {
		return false
	}
	if selected := normalizePriceRefGPUName(chosen.GPUName); selected != "" {
		return normalizePriceRefGPUName(offer.GPUName) == selected
	}
	if required := normalizePriceRefGPUName(group.GPUClass); required != "" && required != normalizePriceRefGPUName("nvidia") {
		return normalizePriceRefGPUName(offer.GPUName) == required
	}
	return false
}

func normalizePriceRefGPUName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func priceCentsCeil(pricePerHour float64) int {
	if pricePerHour <= 0 {
		return 0
	}
	return int(math.Ceil(pricePerHour*100 - 1e-9))
}

func offerConstraintsForGroup(group InstanceGroup, minReliability float64) cloud.OfferConstraints {
	c := cloud.OfferConstraints{
		GPUClass:         group.GPUClass,
		MinGPUMemGB:      group.GPUMemGB,
		MinDiskGB:        group.DiskGB,
		MinReliability:   minReliability,
		MinDriverVersion: group.MinDriverVersion,
		MinCUDAVersion:   group.MinCUDAVersion,
		NumGPUs:          requiredGPUCountForGroup(group),
		Interconnect:     strings.TrimSpace(group.Interconnect),
		RunpodCloudType:  strings.TrimSpace(group.RunpodCloudType),
		// Legacy GPU memory upper metadata is intentionally not passed to search.
		// Offer selection relies on cost/runtime scoring after the hard
		// minimum compatibility filters.
	}
	if group.HasComputeIntensiveJob() {
		c.MinCPUCoresEffective = computeCPUCoresFloor()
	}
	if group.CPUCores > c.MinCPUCoresEffective {
		c.MinCPUCoresEffective = group.CPUCores
	}
	c.MinHostRAMGB = group.CPUMemGB
	if group.HasPreemptibleJob() && !group.EscalateToOnDemand {
		c.InstanceType = cloud.InstanceTypeInterruptible
	}
	return c
}

const defaultComputeCPUCores = 16

func computeCPUCoresFloor() int {
	return intFromEnvOrDefault("WEFT_COMPUTE_CPU_CORES", defaultComputeCPUCores)
}

func intFromEnvOrDefault(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return defaultVal
}

// filterOffersByCUDACompat removes offers whose GPU requires a newer CUDA
// toolkit than the Docker image provides. Returns the compatible offers and
// the count of filtered offers (for logging).
func filterOffersByCUDACompat(offers []cloud.Offer, image string) ([]cloud.Offer, int, float64, float64, string, string) {
	if image == "" {
		image = cloud.DefaultImage
	}
	cudaVer, _, ok := parseCUDAImage(image)
	if !ok {
		return offers, 0, 0, 0, "", image
	}
	imageCUDA := parseCUDAVersionFloat(cudaVer)
	if imageCUDA == 0 {
		return offers, 0, 0, 0, "", image
	}

	compatible := make([]cloud.Offer, 0, len(offers))
	filtered := 0
	var maxRequired float64
	exampleGPU := ""
	for _, o := range offers {
		minCUDA := placement.MinCUDAForGPU(o.GPUName)
		if minCUDA > 0 && minCUDA > imageCUDA {
			filtered++
			if minCUDA > maxRequired {
				maxRequired = minCUDA
				exampleGPU = o.GPUName
			}
			continue
		}
		compatible = append(compatible, o)
	}
	if filtered > 0 {
		slog.Debug("filtered offers by CUDA compatibility",
			"image_cuda", imageCUDA, "filtered", filtered, "remaining", len(compatible))
	}
	return compatible, filtered, imageCUDA, maxRequired, exampleGPU, image
}

const unknownCompatibilityPenalty = 1_000_000_000

// CompatibilityStatus is the three-state offer compatibility result plus the
// explicit no-requirement case.
type CompatibilityStatus string

const (
	CompatibilityKnown        CompatibilityStatus = "known"
	CompatibilityUnknown      CompatibilityStatus = "unknown"
	CompatibilityIncompatible CompatibilityStatus = "incompatible"
	CompatibilityNotRequired  CompatibilityStatus = "not_required"
)

func groupRequiresProviderCompatibility(group InstanceGroup) bool {
	return strings.TrimSpace(group.MinCUDAVersion) != "" || group.MinDriverVersion > 0
}

func offerCompatibilityStatus(group InstanceGroup, offer cloud.Offer) CompatibilityStatus {
	if !groupRequiresProviderCompatibility(group) {
		return CompatibilityNotRequired
	}
	// Evaluate the CUDA compatibility chain links this site has values for
	// (specs/campaign-lifecycle.allium contract CUDACompatibilityChain):
	// the group's merged CUDA floor and image toolkit against the offer's
	// driver-supported CUDA (cuda_max_good). Offers that don't report a
	// CUDA version fall through to the known/unknown provider handling.
	if offer.CUDAVersion > 0 {
		chain := compat.CUDAChain{
			CUDAFloor:  strings.TrimSpace(group.MinCUDAVersion),
			DriverCUDA: strconv.FormatFloat(offer.CUDAVersion, 'f', -1, 64),
		}
		if imageCUDA, _, ok := parseCUDAImage(group.Image); ok {
			chain.ImageToolkit = imageCUDA
		}
		if compat.ValidateCUDAChain(chain) != nil {
			return CompatibilityIncompatible
		}
	}
	switch offer.Provider {
	case cloud.ProviderVastai:
		// Vast.ai search applies driver_version/cuda_vers predicates before
		// returning offers, so a returned offer is known compatible even though
		// the provider-neutral Offer does not carry driver_version.
		return CompatibilityKnown
	case cloud.ProviderRunpod:
		return CompatibilityUnknown
	default:
		if offer.CUDAVersion > 0 && strings.TrimSpace(group.MinCUDAVersion) != "" {
			return CompatibilityKnown
		}
		return CompatibilityUnknown
	}
}

func filterOffersByProviderCompatibility(group InstanceGroup, offers []cloud.Offer) ([]cloud.Offer, int, int) {
	if !groupRequiresProviderCompatibility(group) {
		return offers, 0, 0
	}
	knownCompatible := make([]cloud.Offer, 0, len(offers))
	unknownCompatible := make([]cloud.Offer, 0, len(offers))
	filtered := 0
	unknown := 0
	for _, offer := range offers {
		switch offerCompatibilityStatus(group, offer) {
		case CompatibilityIncompatible:
			filtered++
		case CompatibilityUnknown:
			unknown++
			unknownCompatible = append(unknownCompatible, offer)
		default:
			knownCompatible = append(knownCompatible, offer)
		}
	}
	if len(knownCompatible) > 0 {
		return knownCompatible, filtered + len(unknownCompatible), unknown
	}
	// Unknown-compatible providers (currently RunPod) do not expose
	// driver_version / cuda_max_good at offer search. If there is no
	// known-compatible offer, keep them as a high-penalty fallback and rely on
	// the launch-time driver probe to reject incompatible hosts quickly.
	return unknownCompatible, filtered, unknown
}

func filterOffersByForwardCompatDriver(group InstanceGroup, offers []cloud.Offer) ([]cloud.Offer, int, string) {
	if !groupRequiresProviderCompatibility(group) {
		return offers, 0, ""
	}
	compatible := make([]cloud.Offer, 0, len(offers))
	filtered := 0
	exampleGPU := ""
	for _, offer := range offers {
		if offer.Provider == cloud.ProviderVastai && offer.DatacenterDriver && consumerNVIDIAGPU(offer.GPUName) {
			filtered++
			if exampleGPU == "" {
				exampleGPU = offer.GPUName
			}
			continue
		}
		compatible = append(compatible, offer)
	}
	return compatible, filtered, exampleGPU
}

func consumerNVIDIAGPU(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.Contains(n, "geforce") || strings.Contains(n, "gtx") {
		return true
	}
	normalized := strings.NewReplacer("_", " ", "-", " ").Replace(n)
	fields := strings.Fields(normalized)
	for i, field := range fields {
		if field == "rtx" && i+1 < len(fields) && isConsumerRTXNumber(fields[i+1]) {
			return true
		}
		if strings.HasPrefix(field, "rtx") && isConsumerRTXNumber(strings.TrimPrefix(field, "rtx")) {
			return true
		}
	}
	return false
}

func isConsumerRTXNumber(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	switch s[:2] {
	case "20", "30", "40", "50":
		return true
	default:
		return false
	}
}

func compatibilityScorePenalty(group InstanceGroup, offer cloud.Offer) float64 {
	if offerCompatibilityStatus(group, offer) == CompatibilityUnknown {
		return unknownCompatibilityPenalty
	}
	return 0
}

// filterOffersByTorchArch removes offers whose GPU compute capability is
// outside the bounds implied by the group's torch pin (or explicit
// gpu-arch-max).
// Returns the surviving offers, the count filtered, and one example GPU+cap
// for diagnostics.
func filterOffersByTorchArch(offers []cloud.Offer, minCap, maxCap string) ([]cloud.Offer, int, string, string) {
	if minCap == "" && maxCap == "" {
		return offers, 0, "", ""
	}
	compatible := make([]cloud.Offer, 0, len(offers))
	filtered := 0
	exampleGPU, exampleCap := "", ""
	constraints := placement.Constraints{MinComputeCap: minCap, MaxComputeCap: maxCap}
	for _, o := range offers {
		if !placement.EvaluateEligibility(constraints, TargetSpecFromOffer(o)).Eligible {
			filtered++
			if exampleGPU == "" {
				exampleGPU = o.GPUName
				exampleCap = placement.ComputeCapForGPU(o.GPUName)
				if exampleCap == "" {
					exampleCap = "unknown"
				}
			}
			continue
		}
		compatible = append(compatible, o)
	}
	if filtered > 0 {
		slog.Debug("filtered offers by torch arch bounds",
			"min_cap", minCap, "max_cap", maxCap, "filtered", filtered,
			"remaining", len(compatible), "example_gpu", exampleGPU)
	}
	return compatible, filtered, exampleGPU, exampleCap
}

// filterOffersByVRAMReq applies a defensive local VRAM minimum filter.
// We still rely on provider-side filtering, but this guards against provider
// inconsistencies. Legacy GPU memory upper metadata is not enforced here.
func filterOffersByVRAMReq(offers []cloud.Offer, group InstanceGroup) ([]cloud.Offer, int) {
	if len(offers) == 0 {
		return offers, 0
	}

	constraints := placement.Constraints{GPUMemGB: group.GPUMemGB}
	filtered := make([]cloud.Offer, 0, len(offers))
	removed := 0
	for _, o := range offers {
		if !placement.EvaluateEligibility(constraints, TargetSpecFromOffer(o)).Eligible {
			removed++
			continue
		}
		filtered = append(filtered, o)
	}
	return filtered, removed
}

func requiredGPUCountForGroup(group InstanceGroup) int {
	return normalizedGPUCount(group.NumGPUs)
}

func offerHasInsufficientGPUCount(group InstanceGroup, offer cloud.Offer) (required, actual int, insufficient bool) {
	required = requiredGPUCountForGroup(group)
	actual = offer.NumGPUs
	return required, actual, required > 1 && actual > 0 && actual < required
}

func validateOfferGPUCount(group InstanceGroup, offer cloud.Offer) error {
	required, actual, insufficient := offerHasInsufficientGPUCount(group, offer)
	if !insufficient {
		return nil
	}
	return fmt.Errorf("offer GPU count insufficient: need=%d offer=%d", required, actual)
}

func filterOffersByGPUCount(offers []cloud.Offer, group InstanceGroup) ([]cloud.Offer, int) {
	if len(offers) == 0 {
		return offers, 0
	}
	required := requiredGPUCountForGroup(group)
	if required <= 1 {
		return offers, 0
	}
	filtered := make([]cloud.Offer, 0, len(offers))
	removed := 0
	constraints := placement.Constraints{NumGPUs: required}
	for _, o := range offers {
		if o.NumGPUs > 0 && !placement.EvaluateEligibility(constraints, TargetSpecFromOffer(o)).Eligible {
			removed++
			continue
		}
		filtered = append(filtered, o)
	}
	return filtered, removed
}

// filterOffersByHostRAM drops offers whose host/system RAM is below the group's
// declared --cpu-mem floor. Offers with unknown host RAM (RAMGB == 0, e.g. every
// RunPod offer, which carries no host-RAM field) are kept: weft has no signal to
// judge them on, so excluding them would silently drop the whole provider.
func filterOffersByHostRAM(offers []cloud.Offer, group InstanceGroup) ([]cloud.Offer, int) {
	if len(offers) == 0 || group.CPUMemGB <= 0 {
		return offers, 0
	}
	filtered := make([]cloud.Offer, 0, len(offers))
	removed := 0
	for _, o := range offers {
		if o.RAMGB > 0 && o.RAMGB < group.CPUMemGB {
			removed++
			continue
		}
		filtered = append(filtered, o)
	}
	return filtered, removed
}

func filterOffersByInterconnect(offers []cloud.Offer, group InstanceGroup) ([]cloud.Offer, int) {
	req := strings.ToLower(strings.TrimSpace(group.Interconnect))
	if len(offers) == 0 || req == "" || req == "any" {
		return offers, 0
	}
	filtered := make([]cloud.Offer, 0, len(offers))
	removed := 0
	for _, o := range offers {
		name := strings.ToLower(o.GPUName + " " + o.DataCenter)
		hasNVLinkSignal := strings.Contains(name, "nvlink") || strings.Contains(name, "sxm")
		switch req {
		case "nvlink":
			if !hasNVLinkSignal {
				removed++
				continue
			}
		case "pcie":
			if hasNVLinkSignal {
				removed++
				continue
			}
		}
		filtered = append(filtered, o)
	}
	return filtered, removed
}

// rankOffer selects the best offer from a slice and returns a GroupOffer.
func rankOffer(group InstanceGroup, offers []cloud.Offer, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupOverhead bidding.OfferSetupFunc, strategy bidding.SelectionStrategy, minSurvival float64) GroupOffer {
	return rankOfferWithProfile(group, offers, survivalModel, jobDurationHrs, setupOverhead, strategy.Profile(), minSurvival)
}

func rankOfferWithProfile(group InstanceGroup, offers []cloud.Offer, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupOverhead bidding.OfferSetupFunc, profile bidding.ScoreProfile, minSurvival float64) GroupOffer {
	result := GroupOffer{Group: group}
	stats := OfferFilterStats{RawCount: len(offers)}
	minSurvival = db.RequestedMinSurvivalForJobs(group.Jobs, minSurvival)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result
	}
	if vramFiltered, removed := filterOffersByVRAMReq(offers, group); removed > 0 {
		slog.Debug("filtered offers by VRAM requirement",
			"required_min_gb", group.GPUMemGB,
			"filtered", removed, "remaining", len(vramFiltered))
		offers = vramFiltered
	}
	stats.AfterVRAM = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result
	}
	if countFiltered, removed := filterOffersByGPUCount(offers, group); removed > 0 {
		slog.Debug("filtered offers by GPU count requirement",
			"required", requiredGPUCountForGroup(group),
			"filtered", removed, "remaining", len(countFiltered))
		stats.GPUCountFiltered = removed
		offers = countFiltered
	}
	stats.AfterGPUCount = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result
	}
	if ramFiltered, removed := filterOffersByHostRAM(offers, group); removed > 0 {
		slog.Debug("filtered offers by host RAM requirement",
			"required_min_gb", group.CPUMemGB,
			"filtered", removed, "remaining", len(ramFiltered))
		stats.HostRAMFiltered = removed
		offers = ramFiltered
	}
	stats.AfterHostRAM = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result
	}
	if topoFiltered, removed := filterOffersByInterconnect(offers, group); removed > 0 {
		slog.Debug("filtered offers by interconnect requirement",
			"interconnect", group.Interconnect,
			"filtered", removed, "remaining", len(topoFiltered))
		stats.InterconnectFiltered = removed
		stats.InterconnectRequired = strings.TrimSpace(group.Interconnect)
		offers = topoFiltered
	}
	stats.AfterInterconnect = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result
	}
	offers, cudaFiltered, imageCUDA, minRequiredCUDA, exampleGPU, imageRef := filterOffersByCUDACompat(offers, group.Image)
	if cudaFiltered > 0 {
		stats.CUDAImage = imageRef
		stats.CUDAImageVersion = imageCUDA
		stats.CUDAMinRequired = minRequiredCUDA
		stats.CUDAExampleGPU = exampleGPU
	}
	stats.AfterCUDA = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result
	}
	offers, providerCompatFiltered, providerCompatUnknown := filterOffersByProviderCompatibility(group, offers)
	if providerCompatFiltered > 0 {
		slog.Debug("filtered offers by provider CUDA compatibility",
			"filtered", providerCompatFiltered, "remaining", len(offers))
	}
	stats.UnknownCompatibility = providerCompatUnknown
	stats.ProviderCompatibilityFiltered = providerCompatFiltered
	stats.AfterProvider = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result
	}
	offers, forwardCompatFiltered, forwardCompatExampleGPU := filterOffersByForwardCompatDriver(group, offers)
	if forwardCompatFiltered > 0 {
		slog.Debug("filtered offers by Vast.ai datacenter forward-compat driver guard",
			"filtered", forwardCompatFiltered, "remaining", len(offers), "example_gpu", forwardCompatExampleGPU)
	}
	stats.ForwardCompatFiltered = forwardCompatFiltered
	stats.ForwardCompatExampleGPU = forwardCompatExampleGPU
	stats.AfterForward = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result
	}
	if group.MinComputeCap != "" || group.MaxComputeCap != "" {
		archOffers, archFiltered, exampleGPU, exampleCap := filterOffersByTorchArch(offers, group.MinComputeCap, group.MaxComputeCap)
		stats.TorchArchMinCap = group.MinComputeCap
		stats.TorchArchMaxCap = group.MaxComputeCap
		if archFiltered > 0 {
			stats.TorchArchExampleGPU = exampleGPU
			stats.TorchArchExampleCap = exampleCap
		}
		offers = archOffers
	}
	stats.AfterTorchArch = len(offers)
	if len(offers) == 0 {
		result.FilterStats = stats
		return result
	}
	filtered, rejected := bidding.FilterOffersBySurvival(survivalModel, offers, minSurvival)
	result.RejectedGroups = rejected
	stats.AfterSurvival = len(filtered)
	if len(filtered) == 0 {
		result.FilterStats = stats
		return result
	}
	totalJobDurationHrs := jobDurationHrs
	if len(group.Jobs) > 1 {
		totalJobDurationHrs *= float64(len(group.Jobs))
	}
	_, best := bestOfferWithCompatibilityProfile(group, survivalModel, filtered, totalJobDurationHrs, len(group.Jobs), setupOverhead, profile)
	result.Offer = &best
	if survivalModel != nil {
		result.SurvivalProb = survivalModel.OfferSurvival(best)
	}
	result.Alternatives = rankNeutralOfferAlternatives(group, filtered, survivalModel, totalJobDurationHrs, len(group.Jobs), setupOverhead, profile, best)
	result.FilterStats = stats
	return result
}

func bestOfferWithCompatibilityProfile(group InstanceGroup, model *bidding.SurvivalModel, offers []cloud.Offer, totalRunHrs float64, jobCount int, setupOverhead bidding.OfferSetupFunc, profile bidding.ScoreProfile) (int, cloud.Offer) {
	bestIdx := -1
	bestScore := math.Inf(1)
	w := profile.Weights()
	for i, offer := range offers {
		setup := setupOverhead(offer)
		surv := 1.0
		if model != nil {
			surv = model.OfferSurvival(offer)
		}
		cost := bidding.ExpectedCost(offer.CostPerHour, totalRunHrs, setup, surv)
		completionHrs := float64(jobCount)*setup + totalRunHrs
		if !profile.UseHappyPathTime && surv > 0 && surv < 1 {
			completionHrs /= surv
		}
		score := w.Cost*cost + w.Time*completionHrs + compatibilityScorePenalty(group, offer)
		if bestIdx < 0 || score < bestScore {
			bestIdx = i
			bestScore = score
		}
	}
	if bestIdx < 0 {
		return -1, cloud.Offer{}
	}
	return bestIdx, offers[bestIdx]
}

func rankNeutralOfferAlternatives(group InstanceGroup, offers []cloud.Offer, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, jobCount int, setupOverhead bidding.OfferSetupFunc, profile bidding.ScoreProfile, selected cloud.Offer) []RankedOfferAlternative {
	w := profile.Weights()
	alternatives := make([]RankedOfferAlternative, 0, len(offers))
	for _, offer := range offers {
		setup := setupOverhead(offer)
		surv := 1.0
		if survivalModel != nil {
			surv = survivalModel.OfferSurvival(offer)
		}
		cost := bidding.ExpectedCost(offer.CostPerHour, jobDurationHrs, setup, surv)
		completionHrs := float64(jobCount)*setup + jobDurationHrs
		if !profile.UseHappyPathTime && surv > 0 && surv < 1 {
			completionHrs /= surv
		}
		compatibility := offerCompatibilityStatus(group, offer)
		alternatives = append(alternatives, RankedOfferAlternative{
			Offer:         offer,
			Score:         w.Cost*cost + w.Time*completionHrs + compatibilityScorePenalty(group, offer),
			CompletionHrs: completionHrs,
			Cost:          cost,
			Survival:      surv,
			Compatibility: compatibility,
			Selected:      offer.Key() == selected.Key(),
		})
	}
	sort.Slice(alternatives, func(i, j int) bool {
		return alternatives[i].Score < alternatives[j].Score
	})
	for i := range alternatives {
		alternatives[i].Rank = i + 1
	}
	return alternatives
}

// SearchBestOfferForGroup searches cloud providers for the best offer matching a
// group's requirements, optionally excluding previously failed offer IDs.
func SearchBestOfferForGroup(
	clients []cloud.Client,
	group InstanceGroup,
	survivalModel *bidding.SurvivalModel,
	jobDurationHrs float64,
	setupOverhead bidding.OfferSetupFunc,
	excludeOfferIDs map[string]struct{},
	strategy bidding.SelectionStrategy,
	minReliability float64,
	minSurvival float64,
) GroupOffer {
	return SearchBestOfferForGroupWithProfile(clients, group, survivalModel, jobDurationHrs, setupOverhead, excludeOfferIDs, strategy.Profile(), minReliability, minSurvival)
}

// SearchTopKOffersForGroup returns up to k distinct offers for the
// given group, ranked by the same scoring as SearchBestOfferForGroup.
// The initial exclude set is honored as the seed; each picked offer is
// added to it before the next call. The result may have fewer than k
// entries when the provider lacks enough qualifying offers.
func SearchTopKOffersForGroup(
	clients []cloud.Client,
	group InstanceGroup,
	k int,
	survivalModel *bidding.SurvivalModel,
	jobDurationHrs float64,
	setupOverhead bidding.OfferSetupFunc,
	excludeOfferIDs map[string]struct{},
	strategy bidding.SelectionStrategy,
	minReliability float64,
	minSurvival float64,
) []GroupOffer {
	if k <= 0 {
		return nil
	}
	exclude := map[string]struct{}{}
	for id := range excludeOfferIDs {
		exclude[id] = struct{}{}
	}
	results := make([]GroupOffer, 0, k)
	for i := 0; i < k; i++ {
		gOffer := SearchBestOfferForGroup(clients, group, survivalModel, jobDurationHrs, setupOverhead, exclude, strategy, minReliability, minSurvival)
		if gOffer.Err != nil || gOffer.Offer == nil {
			break
		}
		results = append(results, gOffer)
		exclude[gOffer.Offer.Key()] = struct{}{}
	}
	return results
}

func SearchBestOfferForGroupWithProfile(
	clients []cloud.Client,
	group InstanceGroup,
	survivalModel *bidding.SurvivalModel,
	jobDurationHrs float64,
	setupOverhead bidding.OfferSetupFunc,
	excludeOfferIDs map[string]struct{},
	profile bidding.ScoreProfile,
	minReliability float64,
	minSurvival float64,
) GroupOffer {
	return SearchBestOfferForGroupWithProfileAndMachineExclusions(
		clients,
		group,
		survivalModel,
		jobDurationHrs,
		setupOverhead,
		excludeOfferIDs,
		nil,
		nil,
		profile,
		minReliability,
		minSurvival,
	)
}

func SearchBestOfferForGroupWithProfileAndMachineExclusions(
	clients []cloud.Client,
	group InstanceGroup,
	survivalModel *bidding.SurvivalModel,
	jobDurationHrs float64,
	setupOverhead bidding.OfferSetupFunc,
	excludeOfferIDs map[string]struct{},
	excludeMachineKeys map[string]struct{},
	includeMachineKeys map[string]struct{},
	profile bidding.ScoreProfile,
	minReliability float64,
	minSurvival float64,
) GroupOffer {
	requestedProvider := cloud.Provider(strings.TrimSpace(group.Provider))
	offers, err, _ := searchAllProvidersWithDiagnostics(clients, offerConstraintsForGroup(group, minReliability), requestedProvider)
	if err != nil {
		return GroupOffer{Group: group, Err: err}
	}

	if len(excludeOfferIDs) > 0 {
		filtered := offers[:0]
		for _, offer := range offers {
			if _, excluded := excludeOfferIDs[offer.Key()]; excluded {
				continue
			}
			filtered = append(filtered, offer)
		}
		offers = filtered
	}
	if len(includeMachineKeys) > 0 {
		beforeMachineFilter := len(offers)
		offers = filterOffersByMachineAffinity(offers, includeMachineKeys)
		if beforeMachineFilter > 0 && len(offers) == 0 {
			return GroupOffer{Group: group, Err: ErrMachineAffinityUnsatisfied}
		}
	}
	if len(excludeMachineKeys) > 0 {
		beforeMachineFilter := len(offers)
		filtered := offers[:0]
		for _, offer := range offers {
			if key := offerMachineClaimKey(offer); key != "" {
				if _, excluded := excludeMachineKeys[key]; excluded {
					continue
				}
			}
			filtered = append(filtered, offer)
		}
		if beforeMachineFilter > 0 && len(filtered) == 0 {
			return GroupOffer{Group: group, Err: ErrDistinctMachinesExhausted}
		}
		offers = filtered
	}

	return rankOfferWithProfile(group, offers, survivalModel, jobDurationHrs, setupOverhead, profile, minSurvival)
}

// GroupRawOffers pairs an instance group with all available cloud offers (unranked).
type GroupRawOffers struct {
	Group          InstanceGroup
	Offers         []cloud.Offer
	Err            error
	ProviderErrors []string
}

type offerSearchResult struct {
	offers         []cloud.Offer
	err            error
	providerErrors []string
}

type offerSearchFuture struct {
	done   chan struct{}
	result offerSearchResult
}

// offerSearchSession caches cloud searches by normalized constraints for the
// lifetime of one planning pass.
type offerSearchSession struct {
	clients        []cloud.Client
	minReliability float64
	allowNetwork   bool
	cacheMissErr   error

	mu      sync.Mutex
	results map[string]*offerSearchFuture
}

func newOfferSearchSession(clients []cloud.Client, minReliability float64) *offerSearchSession {
	return newOfferSearchSessionWithOptions(clients, minReliability, true)
}

func newOfferSearchSessionWithOptions(clients []cloud.Client, minReliability float64, allowNetwork bool) *offerSearchSession {
	return &offerSearchSession{
		clients:        clients,
		minReliability: minReliability,
		allowNetwork:   allowNetwork,
		cacheMissErr:   ErrOfferSnapshotUnavailable,
		results:        make(map[string]*offerSearchFuture),
	}
}

func (s *offerSearchSession) SeedRawOffers(raw []GroupRawOffers) {
	if s == nil {
		return
	}
	for _, groupRaw := range raw {
		provider := cloud.Provider(strings.TrimSpace(groupRaw.Group.Provider))
		key := constraintKey(offerConstraintsForGroup(groupRaw.Group, s.minReliability), provider)
		future := &offerSearchFuture{
			done: make(chan struct{}),
			result: offerSearchResult{
				offers: append([]cloud.Offer(nil), groupRaw.Offers...),
				err:    groupRaw.Err,
			},
		}
		close(future.done)

		s.mu.Lock()
		if _, ok := s.results[key]; !ok {
			s.results[key] = future
		}
		if groupRaw.Err != nil && errors.Is(groupRaw.Err, ErrOfferSnapshotUnavailable) {
			s.cacheMissErr = groupRaw.Err
		}
		s.mu.Unlock()
	}
}

func (s *offerSearchSession) fetchGroupRawOffers(groups []InstanceGroup) []GroupRawOffers {
	if len(groups) == 0 {
		return nil
	}
	if len(s.clients) == 0 && s.allowNetwork {
		results := make([]GroupRawOffers, len(groups))
		for i, group := range groups {
			results[i] = GroupRawOffers{Group: group}
		}
		return results
	}

	keys := make([]string, len(groups))
	futures := make(map[string]*offerSearchFuture, len(groups))
	for i, group := range groups {
		constraints := offerConstraintsForGroup(group, s.minReliability)
		provider := cloud.Provider(strings.TrimSpace(group.Provider))
		key := constraintKey(constraints, provider)
		keys[i] = key
		futures[key] = s.getOrStart(key, constraints, provider)
	}

	for _, future := range futures {
		<-future.done
	}

	results := make([]GroupRawOffers, len(groups))
	for i, group := range groups {
		result := futures[keys[i]].result
		results[i] = GroupRawOffers{Group: group, Offers: result.offers, Err: result.err, ProviderErrors: result.providerErrors}
	}
	return results
}

func (s *offerSearchSession) getOrStart(key string, constraints cloud.OfferConstraints, provider cloud.Provider) *offerSearchFuture {
	s.mu.Lock()
	if future, ok := s.results[key]; ok {
		s.mu.Unlock()
		return future
	}
	future := &offerSearchFuture{done: make(chan struct{})}
	s.results[key] = future
	s.mu.Unlock()

	if !s.allowNetwork {
		constraintsText := formatProviderSearchConstraints(constraints)
		future.result.err = fmt.Errorf("%w: cached offer snapshot missing for %s", ErrOfferSnapshotUnavailable, constraintsText)
		close(future.done)
		return future
	}

	go func() {
		constraintsText := formatProviderSearchConstraints(constraints)
		oplog.Log("cloud.offer_search.start",
			oplog.WithDetail(fmt.Sprintf("constraints=%s providers=%d provider=%s", constraintsText, len(s.clients), provider)))

		offers, err, diagnostics := searchAllProvidersWithDiagnostics(s.clients, constraints, provider)
		future.result.offers = offers
		future.result.err = err
		future.result.providerErrors = summarizeProviderSearchErrors(diagnostics)

		totalOffers := 0
		successProviders := 0
		for _, d := range diagnostics {
			if d.err != nil {
				oplog.Log("cloud.offer_search.provider",
					oplog.WithDetail(fmt.Sprintf("provider=%s constraints=%s error=%v", d.provider, constraintsText, d.err)))
				continue
			}
			successProviders++
			totalOffers += len(d.offers)
			oplog.Log("cloud.offer_search.provider",
				oplog.WithDetail(fmt.Sprintf("provider=%s constraints=%s requested_provider=%s offers=%d", d.provider, constraintsText, provider, len(d.offers))))
		}

		if err != nil {
			oplog.Log("cloud.offer_search.done",
				oplog.WithDetail(fmt.Sprintf("constraints=%s requested_provider=%s providers_ok=%d total_offers=%d", constraintsText, provider, successProviders, totalOffers)),
				oplog.WithError(err))
		} else {
			oplog.Log("cloud.offer_search.done",
				oplog.WithDetail(fmt.Sprintf("constraints=%s requested_provider=%s providers_ok=%d total_offers=%d", constraintsText, provider, successProviders, totalOffers)))
		}
		close(future.done)
	}()
	return future
}

// constraintKey returns a string key for deduplicating cloud searches.
// Groups with identical constraints produce identical offers.
func constraintKey(c cloud.OfferConstraints, provider cloud.Provider) string {
	return fmt.Sprintf("%s/%d/%d/%d/%d/%s/%.2f/%d/%d/%s/%s/%s/%s",
		c.GPUClass, c.MinGPUMemGB, c.MaxGPUMemGB, c.MinDiskGB,
		normalizedGPUCount(c.NumGPUs), c.Interconnect, c.MinReliability,
		c.MinCPUCoresEffective, c.MinDriverVersion, c.MinCUDAVersion,
		c.InstanceType, c.RunpodCloudType, provider)
}

// GroupRawOfferCacheKey returns the search-cache key for a planning group.
func GroupRawOfferCacheKey(group InstanceGroup, minReliability float64) string {
	provider := cloud.Provider(strings.TrimSpace(group.Provider))
	return constraintKey(offerConstraintsForGroup(group, minReliability), provider)
}

type providerSearchResult struct {
	provider cloud.Provider
	offers   []cloud.Offer
	err      error
}

func summarizeProviderSearchErrors(results []providerSearchResult) []string {
	if len(results) == 0 {
		return nil
	}
	summaries := make([]string, 0, len(results))
	for _, result := range results {
		if result.err == nil {
			continue
		}
		provider := strings.TrimSpace(string(result.provider))
		if provider == "" {
			provider = "unknown provider"
		}
		summaries = append(summaries, provider+": "+result.err.Error())
	}
	return summaries
}

func formatProviderSearchConstraints(c cloud.OfferConstraints) string {
	parts := []string{}
	if base := FormatOfferConstraints(c); base != "" {
		parts = append(parts, base)
	}
	numGPUs := c.NumGPUs
	if numGPUs == 0 {
		numGPUs = 1
	}
	parts = append(parts, fmt.Sprintf("num_gpus=%d", numGPUs))
	if c.InstanceType != "" {
		parts = append(parts, "instance_type="+c.InstanceType)
	}
	if c.RunpodCloudType != "" {
		parts = append(parts, "runpod_cloud_type="+c.RunpodCloudType)
	}
	if c.Interconnect != "" {
		parts = append(parts, "interconnect="+c.Interconnect)
	}
	if len(c.ExcludeGeos) > 0 {
		geos := append([]string(nil), c.ExcludeGeos...)
		sort.Strings(geos)
		parts = append(parts, "exclude_geos="+strings.Join(geos, ","))
	}
	// These provider-side defaults are always injected by Vast.ai search.
	parts = append(parts, "direct_port_count>=1", "verified=true")
	return strings.Join(parts, " ")
}

func searchAllProvidersWithDiagnostics(clients []cloud.Client, constraints cloud.OfferConstraints, requestedProvider cloud.Provider) ([]cloud.Offer, error, []providerSearchResult) {
	selectedClients := clients
	if requestedProvider != "" {
		selectedClients = nil
		for _, client := range clients {
			if client.Provider() == requestedProvider {
				selectedClients = append(selectedClients, client)
			}
		}
		if len(selectedClients) == 0 {
			available := make([]string, 0, len(clients))
			for _, client := range clients {
				available = append(available, string(client.Provider()))
			}
			sort.Strings(available)
			if len(available) == 0 {
				return nil, fmt.Errorf("requested provider %q is unavailable: no cloud providers configured", requestedProvider), nil
			}
			return nil, fmt.Errorf("requested provider %q is unavailable; configured providers: %s", requestedProvider, strings.Join(available, ", ")), nil
		}
	}

	type result struct {
		provider cloud.Provider
		offers   []cloud.Offer
		err      error
	}
	results := make([]result, len(selectedClients))
	var wg sync.WaitGroup

	for i, c := range selectedClients {
		wg.Add(1)
		go func(idx int, client cloud.Client) {
			defer wg.Done()
			offers, err := client.SearchOffers(constraints)
			results[idx] = result{
				provider: client.Provider(),
				offers:   offers,
				err:      err,
			}
		}(i, c)
	}
	wg.Wait()

	diags := make([]providerSearchResult, len(results))
	var allOffers []cloud.Offer
	var lastErr error
	successCount := 0

	for i, r := range results {
		diags[i] = providerSearchResult{provider: r.provider, offers: r.offers, err: r.err}
		if r.err != nil {
			lastErr = r.err
			continue
		}
		successCount++
		allOffers = append(allOffers, r.offers...)
	}
	if successCount == 0 && lastErr != nil {
		return nil, lastErr, diags
	}
	return allOffers, nil, diags
}

// FetchGroupRawOffers searches cloud providers for all offers per group, in parallel.
// Groups with identical constraints share a single search to avoid redundant API calls.
// Returns unranked offers suitable for caching and later ranking by strategy.
func FetchGroupRawOffers(clients []cloud.Client, groups []InstanceGroup, minReliability float64) []GroupRawOffers {
	return newOfferSearchSession(clients, minReliability).fetchGroupRawOffers(groups)
}

// RecordOfferAvailabilitySnapshots persists compact, rate-limited offer
// availability observations for later stock-availability modeling. It is
// best-effort telemetry: callers must not let a write failure affect planning.
func RecordOfferAvailabilitySnapshots(database *sql.DB, raw []GroupRawOffers, ranked []GroupOffer, minReliability float64) {
	if database == nil || len(raw) == 0 {
		return
	}
	filteredByKey := make(map[string]int, len(ranked))
	for _, offer := range ranked {
		key := GroupRawOfferCacheKey(offer.Group, minReliability)
		if offer.FilterStats.AfterSurvival > 0 {
			filteredByKey[key] = offer.FilterStats.AfterSurvival
			continue
		}
		if _, ok := filteredByKey[key]; !ok {
			filteredByKey[key] = 0
		}
	}
	for _, groupRaw := range raw {
		if groupRaw.Err != nil {
			continue
		}
		key := GroupRawOfferCacheKey(groupRaw.Group, minReliability)
		postFilter := filteredByKey[key]
		snapshot := offerAvailabilitySnapshotForGroup(groupRaw.Group, groupRaw.Offers, minReliability, &postFilter, groupRaw.ProviderErrors)
		if err := db.RecordOfferAvailabilitySnapshot(database, snapshot); err != nil {
			slog.Debug("record offer availability snapshot failed", "component", "campaign", "error", err)
		}
	}
}

func offerAvailabilitySnapshotForGroup(group InstanceGroup, offers []cloud.Offer, minReliability float64, postFilterCount *int, providerErrors []string) db.OfferAvailabilitySnapshot {
	minCents, medianCents, p75Cents := OfferPriceQuantilesCents(offers)
	constraints := offerConstraintsForGroup(group, minReliability)
	return db.OfferAvailabilitySnapshot{
		Provider:         strings.TrimSpace(group.Provider),
		GPUClass:         strings.TrimSpace(constraints.GPUClass),
		GPUMemBucketGB:   constraints.MinGPUMemGB,
		DiskBucketGB:     constraints.MinDiskGB,
		NumGPUs:          normalizedGPUCount(constraints.NumGPUs),
		Interconnect:     strings.TrimSpace(constraints.Interconnect),
		InstanceType:     strings.TrimSpace(constraints.InstanceType),
		RunpodCloudType:  strings.TrimSpace(constraints.RunpodCloudType),
		MinReliability:   minReliability,
		OfferCount:       len(offers),
		PostFilterCount:  postFilterCount,
		PriceMinCents:    positiveIntPtr(minCents),
		PriceMedianCents: positiveIntPtr(medianCents),
		PriceP75Cents:    positiveIntPtr(p75Cents),
		Details:          offerAvailabilityDetails(offers, providerErrors),
	}
}

func offerAvailabilityDetails(offers []cloud.Offer, providerErrors []string) map[string]any {
	counts := make(map[string]int)
	for _, offer := range offers {
		provider := strings.TrimSpace(string(offer.Provider))
		if provider == "" {
			provider = "unknown"
		}
		counts[provider]++
	}
	details := map[string]any{
		"provider_offer_counts": counts,
	}
	if len(providerErrors) > 0 {
		details["provider_errors"] = append([]string(nil), providerErrors...)
	}
	return details
}

// OfferPriceQuantilesCents returns min, median, and p75 $/hr prices in cents
// for currently visible offers. Zero values mean no priced offers were present.
func OfferPriceQuantilesCents(offers []cloud.Offer) (int, int, int) {
	if len(offers) == 0 {
		return 0, 0, 0
	}
	cents := make([]int, 0, len(offers))
	for _, offer := range offers {
		if offer.CostPerHour <= 0 {
			continue
		}
		cents = append(cents, int(offer.CostPerHour*100+0.5))
	}
	if len(cents) == 0 {
		return 0, 0, 0
	}
	sort.Ints(cents)
	return cents[0], percentileSortedInts(cents, 0.5), percentileSortedInts(cents, 0.75)
}

func percentileSortedInts(values []int, p float64) int {
	if len(values) == 0 {
		return 0
	}
	if len(values) == 1 {
		return values[0]
	}
	if p <= 0 {
		return values[0]
	}
	if p >= 1 {
		return values[len(values)-1]
	}
	idx := int(math.Ceil(p*float64(len(values)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
}

func positiveIntPtr(v int) *int {
	if v <= 0 {
		return nil
	}
	return &v
}

// SetupOverheadFactory builds per-group OfferSetupFuncs. If nil, a constant
// 0.5h fallback is used for all groups.
type SetupOverheadFactory func(group InstanceGroup) bidding.OfferSetupFunc

// RankGroupOffers selects the best offer per group from cached raw offers using
// the given strategy. The setupFactory creates per-group setup overhead
// functions that account for datacenter download speed and group-specific
// download sizes. If nil, a constant 0.5h fallback is used.
func RankGroupOffers(raw []GroupRawOffers, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupFactory SetupOverheadFactory, strategy bidding.SelectionStrategy, minSurvival float64) []GroupOffer {
	return RankGroupOffersWithProfile(raw, survivalModel, jobDurationHrs, setupFactory, strategy.Profile(), minSurvival)
}

// RankGroupOffersWithPredictor ranks offers using predictor-backed runtimes when
// available. When the predictor cannot model a job, feasible offers fall back to
// equal runtime so cost, setup, and survival break ties.
func RankGroupOffersWithPredictor(raw []GroupRawOffers, predCfg *predictor.Config, survivalModel *bidding.SurvivalModel, setupFactory SetupOverheadFactory, strategy bidding.SelectionStrategy, minSurvival float64) []GroupOffer {
	return rankGroupOffersForPlanning(raw, predCfg, survivalModel, setupFactory, strategy.Profile(), minSurvival)
}

func RankGroupOffersWithProfile(raw []GroupRawOffers, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupFactory SetupOverheadFactory, profile bidding.ScoreProfile, minSurvival float64) []GroupOffer {
	return RankGroupOffersWithProfileAndMachineExclusions(raw, survivalModel, jobDurationHrs, setupFactory, profile, minSurvival, nil, nil)
}

func RankGroupOffersWithProfileAndMachineExclusions(raw []GroupRawOffers, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupFactory SetupOverheadFactory, profile bidding.ScoreProfile, minSurvival float64, initialClaimedMachines map[string]struct{}, machineAffinity map[string]struct{}) []GroupOffer {
	results := make([]GroupOffer, len(raw))
	// See rankGroupOffersFromPredictions for the per-pass claim rationale:
	// excluding offers claimed by earlier groups in this same ranking pass
	// prevents two groups from picking the same machine and racing each
	// other on vastai's per-machine-serialized create endpoint.
	claimedMachines := cloneStringSet(initialClaimedMachines)
	claimedOffers := make(map[string]struct{})
	for i, r := range raw {
		if r.Err != nil {
			results[i] = GroupOffer{Group: r.Group, Err: r.Err}
			continue
		}
		setupOverhead := bidding.ConstantSetup(0.5)
		if setupFactory != nil {
			setupOverhead = setupFactory(r.Group)
		}
		affinityOffers := filterOffersByMachineAffinity(r.Offers, machineAffinity)
		if len(r.Offers) > 0 && len(affinityOffers) == 0 && len(machineAffinity) > 0 {
			results[i] = GroupOffer{Group: r.Group, Err: ErrMachineAffinityUnsatisfied}
			continue
		}
		availableOffers := filterOffersByClaim(affinityOffers, claimedMachines, claimedOffers, false)
		if len(r.Offers) > 0 && len(availableOffers) == 0 && len(claimedMachines) > 0 {
			results[i] = GroupOffer{Group: r.Group, Err: ErrDistinctMachinesExhausted}
			continue
		}
		results[i] = rankOfferWithProfile(r.Group, availableOffers, survivalModel, jobDurationHrs, setupOverhead, profile, minSurvival)
		attachProviderErrors(&results[i], r.ProviderErrors)
		if results[i].Offer != nil {
			recordOfferClaim(*results[i].Offer, claimedMachines, claimedOffers, false)
		}
	}
	return results
}

func attachProviderErrors(offer *GroupOffer, providerErrors []string) {
	if offer == nil || len(providerErrors) == 0 {
		return
	}
	offer.FilterStats.ProviderErrors = append([]string(nil), providerErrors...)
}

// MedianDLPerfFromRawOffers computes the median DLPerf across all raw offers.
// Returns 0 if no offers have DLPerf data.
func MedianDLPerfFromRawOffers(rawOffers []GroupRawOffers) float64 {
	var all []cloud.Offer
	for _, r := range rawOffers {
		all = append(all, r.Offers...)
	}
	return medianDLPerf(all)
}

// MedianDLPerfFromGroupOffers computes the median DLPerf from ranked group offers.
// Returns 0 if no selected offers have DLPerf data.
func MedianDLPerfFromGroupOffers(offers []GroupOffer) float64 {
	var all []cloud.Offer
	for _, o := range offers {
		if o.Offer != nil {
			all = append(all, *o.Offer)
		}
	}
	return medianDLPerf(all)
}

func medianDLPerf(offers []cloud.Offer) float64 {
	hasPerf := false
	for _, o := range offers {
		if o.DLPerf > 0 {
			hasPerf = true
			break
		}
	}
	if !hasPerf {
		return 0
	}
	return bidding.MedianOfferDLPerf(offers)
}

// FetchGroupOffers searches cloud providers for the best offer per group, in parallel.
// When survivalModel is non-nil, selects the offer with lowest expected cost (including
// retry risk from instance failure). Otherwise falls back to cheapest offer.
func FetchGroupOffers(clients []cloud.Client, groups []InstanceGroup, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupFactory SetupOverheadFactory, strategy bidding.SelectionStrategy, minReliability float64, minSurvival float64) []GroupOffer {
	raw := FetchGroupRawOffers(clients, groups, minReliability)
	return RankGroupOffers(raw, survivalModel, jobDurationHrs, setupFactory, strategy, minSurvival)
}

// FetchGroupOffersWithPredictor searches and ranks offers using predictor-backed
// runtimes when available, and a neutral runtime fallback otherwise.
func FetchGroupOffersWithPredictor(clients []cloud.Client, groups []InstanceGroup, predCfg *predictor.Config, survivalModel *bidding.SurvivalModel, setupFactory SetupOverheadFactory, strategy bidding.SelectionStrategy, minReliability float64, minSurvival float64) []GroupOffer {
	raw := FetchGroupRawOffers(clients, groups, minReliability)
	return RankGroupOffersWithPredictor(raw, predCfg, survivalModel, setupFactory, strategy, minSurvival)
}

// GroupingCandidate holds a candidate grouping of jobs with its raw offers.
// The outer search generates multiple candidates (split, merged, reuse) and
// scores each per strategy to find the best grouping.
type GroupingCandidate struct {
	Label  string // e.g. "split", "merged", "reuse"
	Groups []InstanceGroup
	Raw    []GroupRawOffers
}

// FetchCandidateGroupings generates candidate groupings (split, merged,
// parallel) and fetches raw offers for all in parallel. The split groups are
// the input; merged and parallel groups are derived variants.
//
// The "parallel" candidate splits multi-job groups into one-job-per-group,
// using each job's individual GPU memory requirements. This allows the scoring
// function to evaluate running jobs concurrently on separate instances, each
// sized to the individual job's needs.
func FetchCandidateGroupings(clients []cloud.Client, splitGroups []InstanceGroup, minReliability float64) []GroupingCandidate {
	return fetchCandidateGroupingsWithSession(newOfferSearchSession(clients, minReliability), splitGroups)
}

func fetchCandidateGroupingsWithSession(session *offerSearchSession, splitGroups []InstanceGroup) []GroupingCandidate {
	mergedGroups := MergeCompatibleGroups(splitGroups)
	parallelGroups := SplitToParallel(splitGroups)

	// Determine which candidates are distinct
	hasMerged := len(mergedGroups) != len(splitGroups)
	hasParallel := len(parallelGroups) != len(splitGroups)

	if !hasMerged && !hasParallel {
		raw := session.fetchGroupRawOffers(splitGroups)
		return []GroupingCandidate{
			{Label: "split", Groups: splitGroups, Raw: raw},
		}
	}

	// Build candidate list and fetch offers in parallel
	type result struct {
		idx int
		raw []GroupRawOffers
	}

	var candidates []GroupingCandidate
	candidates = append(candidates, GroupingCandidate{Label: "split", Groups: splitGroups})
	if hasMerged {
		candidates = append(candidates, GroupingCandidate{Label: "merged", Groups: mergedGroups})
	}
	if hasParallel {
		candidates = append(candidates, GroupingCandidate{Label: "parallel", Groups: parallelGroups})
	}

	ch := make(chan result, len(candidates))
	for i, cand := range candidates {
		go func(idx int, groups []InstanceGroup) {
			ch <- result{idx, session.fetchGroupRawOffers(groups)}
		}(i, cand.Groups)
	}
	for range candidates {
		r := <-ch
		candidates[r.idx].Raw = r.raw
	}
	return candidates
}

// ScoreGrouping evaluates a candidate grouping for a strategy using the unified
// weighted metric. The cost term sums across groups, while the time term sums
// placed-job completion times, including shared setup and waiting behind
// earlier jobs on the same instance. Returns +Inf if any group has no valid offer.
func ScoreGrouping(groupOffers []GroupOffer, strategy bidding.SelectionStrategy) float64 {
	return ScoreEstimates(ApproximateEstimates(groupOffers), strategy)
}

// BuildReuseCandidate constructs a candidate grouping that assigns jobs to
// existing reusable instances. Each compatible job is matched to the first
// reusable instance that can accept it (GPU, memory, disk). Unmatched jobs
// are excluded (they'll need new instances from other candidates).
//
// The returned candidate has synthetic GroupRawOffers with a single offer per
// group — the existing instance's specs at zero incremental cost (already rented).
func BuildReuseCandidate(jobs []*db.Job, instances []InstanceCapacity) *GroupingCandidate {
	if len(instances) == 0 || len(jobs) == 0 {
		return nil
	}

	// Map instance → assigned jobs
	type assignment struct {
		cap  InstanceCapacity
		jobs []*db.Job
	}
	assignments := make(map[int64]*assignment)
	for _, cap := range instances {
		assignments[cap.Instance.ID] = &assignment{cap: cap}
	}

	// Greedily assign jobs to first compatible instance
	var unmatched int
	for _, job := range jobs {
		matched := false
		for _, cap := range instances {
			if ok, _ := MatchJobToInstance(job, cap); ok {
				assignments[cap.Instance.ID].jobs = append(assignments[cap.Instance.ID].jobs, job)
				matched = true
				break
			}
		}
		if !matched {
			unmatched++
		}
	}

	// If no jobs matched any instance, no reuse candidate
	if unmatched == len(jobs) {
		return nil
	}

	// Build groups and synthetic offers from matched assignments
	var groups []InstanceGroup
	var raw []GroupRawOffers
	for _, a := range assignments {
		if len(a.jobs) == 0 {
			continue
		}
		inst := a.cap.Instance
		group := InstanceGroup{
			GPUClass: inst.GPUClass,
			NumGPUs:  inst.NumGPUs,
			GPUMemGB: inst.GPUMemGB,
			DiskGB:   inst.DiskGB,
			Jobs:     a.jobs,
		}
		// Zero cost: instance is already rented
		syntheticOffer := cloud.Offer{
			ProviderID:  inst.ProviderInstanceID,
			Provider:    cloud.Provider(inst.Provider),
			GPUName:     inst.ResolvedGPUName,
			NumGPUs:     inst.NumGPUs,
			GPUMemGB:    float64(inst.GPUMemGB),
			CostPerHour: 0, // already paying for it
			DLPerf:      inst.DLPerf,
			Reliability: inst.Reliability,
			CPUCores:    inst.CPUCores,
			CPUName:     inst.CPUName,
			RAMGB:       inst.RAMGB,
		}
		groups = append(groups, group)
		raw = append(raw, GroupRawOffers{
			Group:  group,
			Offers: []cloud.Offer{syntheticOffer},
		})
	}

	if len(groups) == 0 {
		return nil
	}

	return &GroupingCandidate{
		Label:  "reuse",
		Groups: groups,
		Raw:    raw,
	}
}

// CandidateResult holds the winning candidate's grouping and offers for a strategy.
type CandidateResult struct {
	CandidateIdx int
	Label        string
	Groups       []InstanceGroup
	Offers       []GroupOffer
	Estimates    []CostEstimate
}

// MapOffersToSplitGroups maps a winning candidate's offers back to the original
// split groups for display. Each split group is assigned the offer from the
// candidate group that contains its jobs. Split groups with no matching
// candidate group get a nil-offer GroupOffer.
func MapOffersToSplitGroups(splitGroups []InstanceGroup, result CandidateResult) []GroupOffer {
	// Build job ID → candidate group index mapping
	jobToCandidate := make(map[int64]int)
	for ci, cg := range result.Groups {
		for _, job := range cg.Jobs {
			jobToCandidate[job.ID] = ci
		}
	}

	mapped := make([]GroupOffer, len(splitGroups))
	for si, sg := range splitGroups {
		mapped[si] = GroupOffer{Group: sg}
		if len(sg.Jobs) == 0 {
			continue
		}
		// Use the first job's candidate group (all jobs in a split group
		// should map to the same candidate group, since merging only
		// combines whole groups)
		if ci, ok := jobToCandidate[sg.Jobs[0].ID]; ok && ci < len(result.Offers) {
			mapped[si].Offer = result.Offers[ci].Offer
			mapped[si].SurvivalProb = result.Offers[ci].SurvivalProb
			mapped[si].RejectedGroups = result.Offers[ci].RejectedGroups
			mapped[si].FilterStats = result.Offers[ci].FilterStats
			mapped[si].Alternatives = result.Offers[ci].Alternatives
			mapped[si].Err = result.Offers[ci].Err
		}
	}
	return mapped
}

// MapEstimatesToSplitGroups maps a winning candidate's estimates back to the
// original split groups using the same job-ID tracing as MapOffersToSplitGroups.
func MapEstimatesToSplitGroups(splitGroups []InstanceGroup, result CandidateResult) []CostEstimate {
	jobToCandidate := make(map[int64]int)
	for ci, cg := range result.Groups {
		for _, job := range cg.Jobs {
			jobToCandidate[job.ID] = ci
		}
	}

	mapped := make([]CostEstimate, len(splitGroups))
	for si, sg := range splitGroups {
		mapped[si] = CostEstimate{Group: sg, Offer: GroupOffer{Group: sg}}
		if len(sg.Jobs) == 0 {
			continue
		}
		ci, ok := jobToCandidate[sg.Jobs[0].ID]
		if !ok || ci >= len(result.Estimates) {
			continue
		}
		mapped[si] = result.Estimates[ci]
		mapped[si].Group = sg
		if ci < len(result.Offers) {
			mapped[si].Offer = result.Offers[ci]
			mapped[si].Offer.Group = sg
		}
	}
	return mapped
}
