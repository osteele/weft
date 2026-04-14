package campaign

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/cloud"
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
// Fields are evaluated in order: VRAM → CUDA → Survival. A zero value means
// either "all filtered out at this stage" or "stage not reached" (because
// an earlier stage already eliminated everything).
type OfferFilterStats struct {
	RawCount      int // offers returned by provider search
	AfterVRAM     int // remaining after VRAM requirement filter
	AfterCUDA     int // remaining after CUDA compatibility filter
	AfterSurvival int // remaining after survival probability filter
}

// NoOffersDetail returns a human-readable explanation of why no offers survived
// filtering. When constraints is non-empty it is appended to the zero-offers
// branch so the caller can see which search predicates yielded nothing.
func (s OfferFilterStats) NoOffersDetail(constraints string) string {
	if s.RawCount == 0 {
		if constraints != "" {
			return fmt.Sprintf("no offers from providers for %s", constraints)
		}
		return "no offers from providers"
	}
	switch {
	case s.AfterVRAM == 0:
		return fmt.Sprintf("%d offers found, all filtered by VRAM requirement", s.RawCount)
	case s.AfterCUDA == 0:
		return fmt.Sprintf("%d offers found, %d passed VRAM but all filtered by CUDA compatibility", s.RawCount, s.AfterVRAM)
	case s.AfterSurvival == 0:
		return fmt.Sprintf("%d offers found, %d passed filters but none met survival threshold", s.RawCount, s.AfterCUDA)
	default:
		// Defensive: all stages passed but no offer was selected.
		return fmt.Sprintf("%d offers found, none met all criteria", s.RawCount)
	}
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
	if c.MaxGPUMemGB > 0 {
		parts = append(parts, fmt.Sprintf("vram<=%dGB", c.MaxGPUMemGB))
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
	return strings.Join(parts, " ")
}

// GroupOffer pairs an instance group with its best cloud offer.
type GroupOffer struct {
	Group          InstanceGroup
	Offer          *cloud.Offer // nil if no offers found
	SurvivalProb   float64      // 0 if no survival model available
	RejectedGroups []bidding.RejectedGroup
	FilterStats    OfferFilterStats
	Err            error
}

func offerConstraintsForGroup(group InstanceGroup) cloud.OfferConstraints {
	c := cloud.OfferConstraints{
		GPUClass:       group.GPUClass,
		MinGPUMemGB:    group.GPUMemGB,
		MinDiskGB:      group.DiskGB,
		MinReliability: cloud.DefaultMinReliability,
		// MaxGPUMemGB is intentionally NOT passed to the search filter.
		// It is a scheduler-side planning hint, not a hard offer filter.
	}
	if group.HasComputeIntensiveJob() {
		c.MinCPUCoresEffective = intFromEnvOrDefault("WEFT_COMPUTE_CPU_CORES", 16)
	}
	if group.HasPreemptibleJob() {
		c.InstanceType = cloud.InstanceTypeInterruptible
	}
	return c
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
func filterOffersByCUDACompat(offers []cloud.Offer, image string) ([]cloud.Offer, int) {
	if image == "" {
		image = cloud.DefaultImage
	}
	cudaVer, _, ok := parseCUDAImage(image)
	if !ok {
		return offers, 0
	}
	imageCUDA := parseCUDAVersionFloat(cudaVer)
	if imageCUDA == 0 {
		return offers, 0
	}

	compatible := make([]cloud.Offer, 0, len(offers))
	filtered := 0
	for _, o := range offers {
		minCUDA := placement.MinCUDAForGPU(o.GPUName)
		if minCUDA > 0 && minCUDA > imageCUDA {
			filtered++
			continue
		}
		compatible = append(compatible, o)
	}
	if filtered > 0 {
		slog.Debug("filtered offers by CUDA compatibility",
			"image_cuda", imageCUDA, "filtered", filtered, "remaining", len(compatible))
	}
	return compatible, filtered
}

// filterOffersByVRAMReq applies a defensive local VRAM minimum filter.
// We still rely on provider-side filtering, but this guards against provider
// inconsistencies. MaxGPUMemGB is not enforced here — it is a soft signal
// used by the bidding system to avoid overpaying for oversized GPUs.
func filterOffersByVRAMReq(offers []cloud.Offer, group InstanceGroup) ([]cloud.Offer, int) {
	if len(offers) == 0 {
		return offers, 0
	}

	const eps = 0.01
	minMem := float64(group.GPUMemGB)

	filtered := make([]cloud.Offer, 0, len(offers))
	removed := 0
	for _, o := range offers {
		if minMem > 0 && o.GPUMemGB+eps < minMem {
			removed++
			continue
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
	defer func() { result.FilterStats = stats }()
	if len(offers) == 0 {
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
		return result
	}
	offers, _ = filterOffersByCUDACompat(offers, group.Image)
	stats.AfterCUDA = len(offers)
	if len(offers) == 0 {
		return result
	}
	filtered, rejected := bidding.FilterOffersBySurvival(survivalModel, offers, minSurvival)
	result.RejectedGroups = rejected
	stats.AfterSurvival = len(filtered)
	if len(filtered) == 0 {
		return result
	}
	totalJobDurationHrs := jobDurationHrs
	if len(group.Jobs) > 1 {
		totalJobDurationHrs *= float64(len(group.Jobs))
	}
	_, best := bidding.BestOfferForJobGroupWithProfile(survivalModel, filtered, totalJobDurationHrs, len(group.Jobs), setupOverhead, profile, group.MaxGPUMemGB)
	result.Offer = &best
	if survivalModel != nil {
		result.SurvivalProb = survivalModel.OfferSurvival(best)
	}
	return result
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
	minSurvival float64,
) GroupOffer {
	return SearchBestOfferForGroupWithProfile(clients, group, survivalModel, jobDurationHrs, setupOverhead, excludeOfferIDs, strategy.Profile(), minSurvival)
}

func SearchBestOfferForGroupWithProfile(
	clients []cloud.Client,
	group InstanceGroup,
	survivalModel *bidding.SurvivalModel,
	jobDurationHrs float64,
	setupOverhead bidding.OfferSetupFunc,
	excludeOfferIDs map[string]struct{},
	profile bidding.ScoreProfile,
	minSurvival float64,
) GroupOffer {
	offers, err := cloud.SearchAllProviders(clients, offerConstraintsForGroup(group))
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

	return rankOfferWithProfile(group, offers, survivalModel, jobDurationHrs, setupOverhead, profile, minSurvival)
}

// GroupRawOffers pairs an instance group with all available cloud offers (unranked).
type GroupRawOffers struct {
	Group  InstanceGroup
	Offers []cloud.Offer
	Err    error
}

type offerSearchResult struct {
	offers []cloud.Offer
	err    error
}

type offerSearchFuture struct {
	done   chan struct{}
	result offerSearchResult
}

// offerSearchSession caches cloud searches by normalized constraints for the
// lifetime of one planning pass.
type offerSearchSession struct {
	clients []cloud.Client

	mu      sync.Mutex
	results map[string]*offerSearchFuture
}

func newOfferSearchSession(clients []cloud.Client) *offerSearchSession {
	return &offerSearchSession{
		clients: clients,
		results: make(map[string]*offerSearchFuture),
	}
}

func (s *offerSearchSession) SeedRawOffers(raw []GroupRawOffers) {
	if s == nil {
		return
	}
	for _, groupRaw := range raw {
		key := constraintKey(offerConstraintsForGroup(groupRaw.Group))
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
		s.mu.Unlock()
	}
}

func (s *offerSearchSession) fetchGroupRawOffers(groups []InstanceGroup) []GroupRawOffers {
	if len(groups) == 0 {
		return nil
	}
	if len(s.clients) == 0 {
		results := make([]GroupRawOffers, len(groups))
		for i, group := range groups {
			results[i] = GroupRawOffers{Group: group}
		}
		return results
	}

	keys := make([]string, len(groups))
	futures := make(map[string]*offerSearchFuture, len(groups))
	for i, group := range groups {
		constraints := offerConstraintsForGroup(group)
		key := constraintKey(constraints)
		keys[i] = key
		futures[key] = s.getOrStart(key, constraints)
	}

	for _, future := range futures {
		<-future.done
	}

	results := make([]GroupRawOffers, len(groups))
	for i, group := range groups {
		result := futures[keys[i]].result
		results[i] = GroupRawOffers{Group: group, Offers: result.offers, Err: result.err}
	}
	return results
}

func (s *offerSearchSession) getOrStart(key string, constraints cloud.OfferConstraints) *offerSearchFuture {
	s.mu.Lock()
	if future, ok := s.results[key]; ok {
		s.mu.Unlock()
		return future
	}
	future := &offerSearchFuture{done: make(chan struct{})}
	s.results[key] = future
	s.mu.Unlock()

	go func() {
		constraintsText := formatProviderSearchConstraints(constraints)
		oplog.Log("cloud.offer_search.start",
			oplog.WithDetail(fmt.Sprintf("constraints=%s providers=%d", constraintsText, len(s.clients))))

		offers, err, diagnostics := searchAllProvidersWithDiagnostics(s.clients, constraints)
		future.result.offers = offers
		future.result.err = err

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
				oplog.WithDetail(fmt.Sprintf("provider=%s constraints=%s offers=%d", d.provider, constraintsText, len(d.offers))))
		}

		if err != nil {
			oplog.Log("cloud.offer_search.done",
				oplog.WithDetail(fmt.Sprintf("constraints=%s providers_ok=%d total_offers=%d", constraintsText, successProviders, totalOffers)),
				oplog.WithError(err))
		} else {
			oplog.Log("cloud.offer_search.done",
				oplog.WithDetail(fmt.Sprintf("constraints=%s providers_ok=%d total_offers=%d", constraintsText, successProviders, totalOffers)))
		}
		close(future.done)
	}()
	return future
}

// constraintKey returns a string key for deduplicating cloud searches.
// Groups with identical constraints produce identical offers.
func constraintKey(c cloud.OfferConstraints) string {
	return fmt.Sprintf("%s/%d/%d/%d/%.2f/%d",
		c.GPUClass, c.MinGPUMemGB, c.MaxGPUMemGB, c.MinDiskGB,
		c.MinReliability, c.MinCPUCoresEffective)
}

type providerSearchResult struct {
	provider cloud.Provider
	offers   []cloud.Offer
	err      error
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
	if len(c.ExcludeGeos) > 0 {
		geos := append([]string(nil), c.ExcludeGeos...)
		sort.Strings(geos)
		parts = append(parts, "exclude_geos="+strings.Join(geos, ","))
	}
	// These provider-side defaults are always injected by Vast.ai search.
	parts = append(parts, "direct_port_count>=1", "verified=true", "gpu_frac=1")
	return strings.Join(parts, " ")
}

func searchAllProvidersWithDiagnostics(clients []cloud.Client, constraints cloud.OfferConstraints) ([]cloud.Offer, error, []providerSearchResult) {
	type result struct {
		provider cloud.Provider
		offers   []cloud.Offer
		err      error
	}
	results := make([]result, len(clients))
	var wg sync.WaitGroup

	for i, c := range clients {
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
func FetchGroupRawOffers(clients []cloud.Client, groups []InstanceGroup) []GroupRawOffers {
	return newOfferSearchSession(clients).fetchGroupRawOffers(groups)
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
	results := make([]GroupOffer, len(raw))
	for i, r := range raw {
		if r.Err != nil {
			results[i] = GroupOffer{Group: r.Group, Err: r.Err}
			continue
		}
		setupOverhead := bidding.ConstantSetup(0.5)
		if setupFactory != nil {
			setupOverhead = setupFactory(r.Group)
		}
		results[i] = rankOfferWithProfile(r.Group, r.Offers, survivalModel, jobDurationHrs, setupOverhead, profile, minSurvival)
	}
	return results
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
func FetchGroupOffers(clients []cloud.Client, groups []InstanceGroup, survivalModel *bidding.SurvivalModel, jobDurationHrs float64, setupFactory SetupOverheadFactory, strategy bidding.SelectionStrategy, minSurvival float64) []GroupOffer {
	raw := FetchGroupRawOffers(clients, groups)
	return RankGroupOffers(raw, survivalModel, jobDurationHrs, setupFactory, strategy, minSurvival)
}

// FetchGroupOffersWithPredictor searches and ranks offers using predictor-backed
// runtimes when available, and a neutral runtime fallback otherwise.
func FetchGroupOffersWithPredictor(clients []cloud.Client, groups []InstanceGroup, predCfg *predictor.Config, survivalModel *bidding.SurvivalModel, setupFactory SetupOverheadFactory, strategy bidding.SelectionStrategy, minSurvival float64) []GroupOffer {
	raw := FetchGroupRawOffers(clients, groups)
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
func FetchCandidateGroupings(clients []cloud.Client, splitGroups []InstanceGroup) []GroupingCandidate {
	return fetchCandidateGroupingsWithSession(newOfferSearchSession(clients), splitGroups)
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
			GPUMemGB: inst.GPUMemGB,
			DiskGB:   inst.DiskGB,
			Jobs:     a.jobs,
		}
		// Zero cost: instance is already rented
		syntheticOffer := cloud.Offer{
			ProviderID:  inst.ProviderInstanceID,
			Provider:    cloud.Provider(inst.Provider),
			GPUName:     inst.ResolvedGPUName,
			GPUMemGB:    float64(inst.GPUMemGB),
			CostPerHour: 0, // already paying for it
			DLPerf:      inst.DLPerf,
			Reliability: inst.Reliability,
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
