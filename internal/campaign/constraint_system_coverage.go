package campaign

import (
	"reflect"
	"slices"

	"github.com/osteele/weft/internal/placement"
)

// PlacementSystem names one of the systems that can bind a job to hardware.
// The offer chain, the reuse matcher, and on-prem scoring each admit work;
// the claim backstop is the write-side assertion inside the DB claim paths.
type PlacementSystem string

const (
	SystemOfferChain    PlacementSystem = "offer-chain"
	SystemReuse         PlacementSystem = "reuse"
	SystemOnPrem        PlacementSystem = "on-prem"
	SystemClaimBackstop PlacementSystem = "claim-backstop"
)

// AllPlacementSystems lists the systems every constraint axis must be
// accounted for in.
func AllPlacementSystems() []PlacementSystem {
	return []PlacementSystem{SystemOfferChain, SystemReuse, SystemOnPrem, SystemClaimBackstop}
}

// systemChecker says how a placement system treats a constraint axis. It is
// the per-system analog of axisChecker, which answers the same question per
// cloud provider within the offer chain.
type systemChecker int

const (
	// checkedInSystem: the system rejects violating targets, through the
	// named predicate.
	checkedInSystem systemChecker = iota
	// notApplicableInSystem: the axis has no meaning in this system, or the
	// system's scope deliberately excludes it — the detail says which and
	// why. This is not a gap: "nothing to check" and "not yet checked" argue
	// for opposite fixes.
	notApplicableInSystem
	// unenforcedSystemGap: the system could check the axis and does not.
	// Every entry in this state must be pinned in
	// TestKnownConstraintSystemGaps, so closing or adding one is a visible,
	// deliberate act.
	unenforcedSystemGap
)

type systemCoverageNote struct {
	by     systemChecker
	detail string
}

// backstopScopeNote explains why most axes carry no claim-time backstop: the
// backstop exists for axes whose violation reads as success. A pinned job
// placed elsewhere completes quickly on hardware nobody asked for; most other
// violations either fail loudly at runtime (arch cap, CUDA floor) or degrade
// visibly (RAM, cores). Widening the backstop is a deliberate act, recorded
// here per axis when taken.
const backstopScopeNote = "backstop reserved for silent-success axes; this one fails visibly downstream"

// containerUserlandNote explains why the glibcxx floor has no meaning off
// prem.
const containerUserlandNote = "cloud jobs run in containers; the host userland is not the job's userland"

// constraintSystemEnforcement records, for every constraint axis derived from
// placement.Constraints' enforce tags, who checks it in each placement
// system. It is the cross-system analog of axisEnforcement (which covers
// providers within the offer chain): the pin bug family happened because a
// constraint could be checked in one system and silently ignored in another,
// and nothing forced the missing decisions to be made. Now a new constraint
// field fails TestConstraintSystemCoverage until each system's row exists.
var constraintSystemEnforcement = map[PlacementSystem]map[string]systemCoverageNote{
	SystemOfferChain: {
		"gpu_class":     {checkedInSystem, "provider search + applyEligibilityFilters (SKU memory, class match); per-provider detail in axisEnforcement"},
		"provider":      {checkedInSystem, "searchAllProvidersWithDiagnostics client selection"},
		"num_gpus":      {checkedInSystem, "filterOffersByGPUCount"},
		"gpu_memory":    {checkedInSystem, "filterOffersByVRAMReq"},
		"cpu_cores":     {checkedInSystem, "effectiveCPUCoresFloor -> MinCPUCoresEffective; RunPod not-applicable per axisEnforcement"},
		"host_ram":      {checkedInSystem, "filterOffersByHostRAM via hostRAMSatisfied (unknown passes)"},
		"interconnect":  {checkedInSystem, "filterOffersByInterconnect via placement.InterconnectSatisfied"},
		"machine_pin":   {checkedInSystem, "effectiveMachineAffinity wrapper filters + TargetSpec.MachineKey in eligibility"},
		"arch_cap_max":  {checkedInSystem, "filterOffersByTorchArch"},
		"arch_cap_min":  {checkedInSystem, "filterOffersByTorchArch"},
		"cuda_floor":    {checkedInSystem, "filterOffersByCUDACompat + filterOffersByProviderCompatibility"},
		"driver_floor":  {checkedInSystem, "provider search predicate (Vast.ai) + RunPod post-create probe"},
		"glibcxx_floor": {notApplicableInSystem, containerUserlandNote},
	},
	SystemReuse: {
		"gpu_class":     {checkedInSystem, "matchInstanceTargetEligibility"},
		"provider":      {checkedInSystem, "matchProviderIntent + providerViolation in EvaluateEligibility"},
		"num_gpus":      {checkedInSystem, "matchInstanceTargetEligibility (unknown instance count relaxes to 1 — fail-open)"},
		"gpu_memory":    {checkedInSystem, "matchInstanceTargetEligibility with IntendedMemGB rollback"},
		"cpu_cores":     {checkedInSystem, "hostCPUCoresSatisfied (unknown fails closed, matching the tag floor)"},
		"host_ram":      {checkedInSystem, "hostRAMSatisfied (unknown passes, matching launch)"},
		"interconnect":  {checkedInSystem, "placement.InterconnectSatisfied over instanceInterconnectSignals"},
		"machine_pin":   {checkedInSystem, "matchMachineAffinityIntent + filterReusableByMachineAffinity"},
		"arch_cap_max":  {checkedInSystem, "matchInstanceTargetEligibility"},
		"arch_cap_min":  {checkedInSystem, "matchInstanceTargetEligibility"},
		"cuda_floor":    {checkedInSystem, "CUDA chain vs Launch.CUDAVersion"},
		"driver_floor":  {checkedInSystem, "EvaluateEligibility vs launches.driver_version; floors derived from any CUDA floor accept recorded CUDA capability evidence, direct driver floors and both-facts-missing targets fail closed (see InstanceReuseHostAxes)"},
		"glibcxx_floor": {notApplicableInSystem, containerUserlandNote},
	},
	SystemOnPrem: {
		"gpu_class":     {checkedInSystem, "EvaluateEligibility device matching"},
		"provider":      {checkedInSystem, "providerViolation in EvaluateEligibility (hosts have no rental provider, so a provider request excludes on-prem)"},
		"num_gpus":      {checkedInSystem, "EvaluateEligibility count + free-GPU accounting"},
		"gpu_memory":    {checkedInSystem, "EvaluateEligibility device memory"},
		"cpu_cores":     {checkedInSystem, "EvaluateEligibility explicit floor; the cpu-intensive tag floor stays cloud-only by design (it avoids paying for weak rentals; on-prem it remains a scoring preference)"},
		"host_ram":      {checkedInSystem, "EvaluateEligibility RAM floor (unknown inventory passes)"},
		"interconnect":  {checkedInSystem, "EvaluateEligibility via placement.InterconnectSatisfied over host GPU naming"},
		"machine_pin":   {checkedInSystem, "machinePinViolation in EvaluateEligibility (hosts have no machine identity, fail closed)"},
		"arch_cap_max":  {checkedInSystem, "EvaluateEligibility compute-cap bounds"},
		"arch_cap_min":  {checkedInSystem, "EvaluateEligibility compute-cap bounds"},
		"cuda_floor":    {checkedInSystem, "targetCompatibilityViolation vs inventory cuda_version; missing fails closed for CUDA-capable or unknown-GPU targets, not applicable to confirmed non-CUDA targets"},
		"driver_floor":  {checkedInSystem, "targetCompatibilityViolation vs inventory nvidia_driver; floors derived from any CUDA floor accept recorded CUDA capability evidence, direct driver floors and both-facts-missing targets fail closed for CUDA-capable or unknown-GPU targets; not applicable to confirmed non-CUDA targets"},
		"glibcxx_floor": {checkedInSystem, "targetCompatibilityViolation vs inventory glibcxx_max (missing fails open)"},
	},
	SystemClaimBackstop: {
		"machine_pin":   {checkedInSystem, "assertMachineAffinitySatisfied (setJobLaunchIDOnce, CreateMoveTargetAttempt) + assertNoMachineAffinityForHost (AssignJobHost, host move targets)"},
		"gpu_class":     {notApplicableInSystem, backstopScopeNote + "; the SKU-memory sub-axis is the standing candidate for a second backstop (the 80GB/40GB incident completed 'successfully')"},
		"provider":      {notApplicableInSystem, backstopScopeNote},
		"num_gpus":      {notApplicableInSystem, backstopScopeNote},
		"gpu_memory":    {notApplicableInSystem, backstopScopeNote},
		"cpu_cores":     {notApplicableInSystem, backstopScopeNote},
		"host_ram":      {notApplicableInSystem, backstopScopeNote},
		"interconnect":  {notApplicableInSystem, backstopScopeNote},
		"arch_cap_max":  {notApplicableInSystem, backstopScopeNote},
		"arch_cap_min":  {notApplicableInSystem, backstopScopeNote},
		"cuda_floor":    {notApplicableInSystem, backstopScopeNote},
		"driver_floor":  {notApplicableInSystem, backstopScopeNote},
		"glibcxx_floor": {notApplicableInSystem, backstopScopeNote},
	},
}

// constraintAxesFromEnforceTags reads the axis vocabulary off
// placement.Constraints, sorted, and reports any exported field carrying no
// axis name (missing or empty enforce tag). It mirrors
// cloud.constraintAxesFromTags, which does the same for OfferConstraints
// within the offer chain.
func constraintAxesFromEnforceTags() (axes []string, untagged []string) {
	for _, field := range reflect.VisibleFields(reflect.TypeOf(placement.Constraints{})) {
		if !field.IsExported() {
			continue
		}
		tag, ok := field.Tag.Lookup("enforce")
		if !ok || tag == "" {
			untagged = append(untagged, field.Name)
			continue
		}
		if tag != "-" {
			axes = append(axes, tag)
		}
	}
	slices.Sort(axes)
	return axes, untagged
}

var allConstraintAxes, untaggedConstraintFields = constraintAxesFromEnforceTags()

// ConstraintAxes returns the axis vocabulary derived from
// placement.Constraints' enforce tags, sorted.
func ConstraintAxes() []string { return slices.Clone(allConstraintAxes) }

// ConstraintSystemReport describes how one placement system treats one
// constraint axis.
type ConstraintSystemReport struct {
	System PlacementSystem
	Axis   string
	Status string // "checked", "not applicable", "NOT ENFORCED", "UNRECORDED"
	Detail string
}

// ExplainConstraintSystems reports how each placement system treats each
// constraint axis, the cross-system counterpart of ExplainAxisEnforcement.
// An axis with no row for a system — the state TestConstraintSystemCoverage
// fails on — is reported as UNRECORDED rather than omitted, so "nobody
// decided" stays visible in the report itself.
func ExplainConstraintSystems() []ConstraintSystemReport {
	systems := AllPlacementSystems()
	axes := ConstraintAxes()
	out := make([]ConstraintSystemReport, 0, len(systems)*len(axes))
	for _, system := range systems {
		byAxis := constraintSystemEnforcement[system]
		for _, axis := range axes {
			report := ConstraintSystemReport{System: system, Axis: axis, Status: "UNRECORDED"}
			if note, ok := byAxis[axis]; ok {
				report.Status = systemStatusLabel(note.by)
				report.Detail = note.detail
			}
			out = append(out, report)
		}
	}
	return out
}

func systemStatusLabel(by systemChecker) string {
	switch by {
	case checkedInSystem:
		return "checked"
	case notApplicableInSystem:
		return "not applicable"
	case unenforcedSystemGap:
		return "NOT ENFORCED"
	}
	return "unknown"
}
