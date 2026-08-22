package placement

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/compat"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/gpucatalog"
	"github.com/osteele/weft/internal/inventory"
)

// TargetSpec is the source-agnostic shape consumed by placement eligibility.
// Constructors normalize inventory hosts, cloud offers, and running rentals
// into this form before constraints are evaluated.
type TargetSpec struct {
	Name              string
	Provider          string
	Devices           []TargetDevice
	Cordoned          bool
	CordonReason      string
	OptInOnly         bool
	ExplicitlyNamed   bool
	LoadKnown         bool
	LoadState         HostLoadState
	LoadReason        string
	Reservations      []GPUReservation
	ReservationsKnown bool
	CPUCores          int
	CPUMemGB          int
	// GPUInventoryKnown means Devices is complete enough to confirm that a
	// target has no CUDA-capable GPU.
	GPUInventoryKnown      bool
	GPUNamingSignals       string
	GPUNamingSignalsKnown  bool
	CUDAVersion            string
	NVIDIADriverVersion    string
	NVIDIADriverMajor      int
	GLIBCXXVersion         string
	Capabilities           []string
	CapabilityAvailability map[string]int
	AgentSlotsRemaining    *int

	// MachineKey is the provider-qualified physical machine identity
	// (db.ProviderMachineKey form, e.g. "vastai/49863"). Empty means the
	// target has no known machine identity: every on-prem host, and cloud
	// targets whose provider did not report one. Pinned jobs fail closed
	// against an empty key — a target that cannot be identified is not
	// confirmed to be the pinned machine.
	MachineKey string

	// MaxComputeCapUnknownFailsClosed keeps source-specific uncertainty policy
	// inside the unified predicate. Unknown max caps fail closed for placement:
	// a max-cap target must have a known device cap at or below the bound.
	MaxComputeCapUnknownFailsClosed bool
}

// TargetDevice describes one homogeneous target GPU pool.
type TargetDevice struct {
	Name          string
	Class         string
	Family        string
	Generation    GPUGeneration
	MemoryGB      int
	ComputeCap    string
	Count         int
	Indices       []int
	ClassAliases  []string
	FullNameAlias []string
}

// Verdict is the structured result of evaluating hard placement eligibility.
type Verdict struct {
	Eligible bool
	Reasons  []EligibilityReason
}

// EligibilityReason identifies one placement eligibility clause result.
type EligibilityReason struct {
	Kind    EligibilityReasonKind
	Message string
	Device  string
	Have    int
	Need    int
}

// EligibilityReasonKind names the hard-constraint clause behind a reason.
type EligibilityReasonKind string

const (
	ReasonCordoned        EligibilityReasonKind = "cordoned"
	ReasonOverloaded      EligibilityReasonKind = "overloaded"
	ReasonOptInOnly       EligibilityReasonKind = "opt_in_only"
	ReasonCompatibility   EligibilityReasonKind = "compatibility"
	ReasonGPUClass        EligibilityReasonKind = "gpu_class"
	ReasonGPUMemory       EligibilityReasonKind = "gpu_memory"
	ReasonGPUCount        EligibilityReasonKind = "gpu_count"
	ReasonGPUAvailability EligibilityReasonKind = "gpu_availability"
	ReasonComputeCapMax   EligibilityReasonKind = "compute_cap_max"
	ReasonComputeCapMin   EligibilityReasonKind = "compute_cap_min"
	ReasonCUDAChain       EligibilityReasonKind = "cuda_chain"
	ReasonMachinePin      EligibilityReasonKind = "machine_pin"
	ReasonProvider        EligibilityReasonKind = "provider"
	ReasonCPUCores        EligibilityReasonKind = "cpu_cores"
	ReasonHostRAM         EligibilityReasonKind = "host_ram"
	ReasonInterconnect    EligibilityReasonKind = "interconnect"
	ReasonCapability      EligibilityReasonKind = "host_capability"
	ReasonCapabilityBusy  EligibilityReasonKind = "host_capability_busy"
)

func (r EligibilityReason) String() string {
	return r.Message
}

// Messages renders the verdict reasons for legacy callers.
func (v Verdict) Messages() []string {
	msgs := make([]string, 0, len(v.Reasons))
	for _, r := range v.Reasons {
		msgs = append(msgs, r.Message)
	}
	return msgs
}

// TargetSpecFromHostSpec normalizes an inventory host for eligibility checks.
func TargetSpecFromHostSpec(host inventory.HostSpec, metrics *HostMetrics, reservations []GPUReservation) TargetSpec {
	t := TargetSpec{
		Name:                            host.Name,
		Devices:                         make([]TargetDevice, 0, len(host.GPUs)),
		Reservations:                    slices.Clone(reservations),
		ReservationsKnown:               reservations != nil,
		CPUCores:                        host.CPUCores,
		CPUMemGB:                        inventory.ParseMemGB(host.Memory),
		GPUInventoryKnown:               true,
		GPUNamingSignals:                hostGPUNameSignals(host),
		GPUNamingSignalsKnown:           true,
		CUDAVersion:                     strings.TrimSpace(host.CUDAVersion),
		NVIDIADriverVersion:             strings.TrimSpace(host.NVIDIADriverVersion),
		NVIDIADriverMajor:               driverMajor(host.NVIDIADriverVersion),
		GLIBCXXVersion:                  strings.TrimSpace(host.GLIBCXXMaxVersion),
		Capabilities:                    slices.Clone(host.Capabilities),
		MaxComputeCapUnknownFailsClosed: true,
	}
	if metrics != nil {
		load := AssessHostLoad(nil, host.Name, metrics, DefaultHostLoadOptions())
		t.LoadKnown = true
		t.LoadState = load.State
		t.LoadReason = load.Reason
	}
	for _, gpu := range host.GPUs {
		t.Devices = append(t.Devices, TargetDeviceFromGPU(gpu.Class, gpu.Name, inventory.ParseMemGB(gpu.Memory), gpuDeviceCount(gpu), gpu.Indices))
	}
	return t
}

// TargetDeviceFromGPU normalizes a GPU name/class pair into a target device.
func TargetDeviceFromGPU(class, name string, memoryGB, count int, indices []int) TargetDevice {
	if count <= 0 {
		count = 1
	}
	class = strings.TrimSpace(class)
	name = strings.TrimSpace(name)
	canonical := canonicalTargetGPUClass(class, name)
	d := TargetDevice{
		Name:       firstNonEmpty(name, class),
		Class:      canonical,
		MemoryGB:   memoryGB,
		Count:      count,
		Indices:    slices.Clone(indices),
		Generation: generationOf(canonical),
	}
	if d.Generation == GenUnknown {
		for _, candidate := range []string{name, class} {
			if gen := generationOf(inventory.NormalizeGPUClass(candidate)); gen != GenUnknown {
				d.Generation = gen
				break
			}
		}
	}
	d.Family = familyForDevice(d.Class, d.Name, d.Generation)
	// Resolve the compute cap from the specific GPU name before the normalized
	// class: the class can collapse to a shorter alias (e.g. "A100" -> "a10"),
	// which would derive the wrong cap. ComputeCapForGPU handles full names.
	d.ComputeCap = ComputeCapForGPU(d.Name)
	if d.ComputeCap == "" {
		d.ComputeCap = ComputeCapForGPU(d.Class)
	}
	d.addClassAlias(class)
	d.addClassAlias(name)
	return d
}

// AddClassAlias records a constructor-time alias that should satisfy exact
// structured GPU constraints for this already-normalized target device.
func (d *TargetDevice) AddClassAlias(alias string) {
	d.addClassAlias(alias)
}

func (d *TargetDevice) addClassAlias(alias string) {
	norm := inventory.NormalizeGPUClass(alias)
	if norm == "" || norm == d.Class {
		return
	}
	if !slices.Contains(d.ClassAliases, norm) {
		d.ClassAliases = append(d.ClassAliases, norm)
	}
}

// AddFullNameAlias records a constructor-time full-name alias.
func (d *TargetDevice) AddFullNameAlias(alias string) {
	alias = strings.TrimSpace(alias)
	if alias == "" || alias == d.Name {
		return
	}
	if !slices.Contains(d.FullNameAlias, alias) {
		d.FullNameAlias = append(d.FullNameAlias, alias)
	}
}

// EvaluateEligibility applies hard placement constraints to a normalized target.
func EvaluateEligibility(c Constraints, t TargetSpec) Verdict {
	v := Verdict{Eligible: true}
	fail := func(kind EligibilityReasonKind, msg string, have, need int) Verdict {
		v.Eligible = false
		v.Reasons = append(v.Reasons, EligibilityReason{Kind: kind, Message: msg, Have: have, Need: need})
		return v
	}

	if t.Cordoned {
		if t.CordonReason != "" {
			return fail(ReasonCordoned, "target cordoned: "+t.CordonReason, 0, 0)
		}
		return fail(ReasonCordoned, "target cordoned", 0, 0)
	}
	if t.LoadKnown && t.LoadState == HostLoadOverloaded {
		if t.LoadReason != "" {
			return fail(ReasonOverloaded, "host overloaded: "+t.LoadReason, 0, 0)
		}
		return fail(ReasonOverloaded, "host overloaded", 0, 0)
	}
	if t.OptInOnly && !t.ExplicitlyNamed {
		return fail(ReasonOptInOnly, "host is opt-in only (specify with --host)", 0, 0)
	}
	for _, required := range c.RequiredCapabilities {
		required = strings.ToLower(strings.TrimSpace(required))
		if required == "" {
			continue
		}
		found := false
		for _, capability := range t.Capabilities {
			if strings.ToLower(strings.TrimSpace(capability)) == required {
				found = true
				break
			}
		}
		if !found {
			return fail(ReasonCapability, fmt.Sprintf("required host capability %q is unavailable", required), 0, 1)
		}
		if strings.HasPrefix(required, "agent:") && t.AgentSlotsRemaining != nil && *t.AgentSlotsRemaining <= 0 {
			return fail(ReasonCapabilityBusy, "authenticated agent worker has no concurrency slots available", *t.AgentSlotsRemaining, 1)
		}
		if remaining, limited := t.CapabilityAvailability[required]; limited && remaining <= 0 {
			return fail(ReasonCapabilityBusy, fmt.Sprintf("required host capability %q has no concurrency slots available", required), remaining, 1)
		}
	}
	if reason, violated := machinePinViolation(c, t); violated {
		return fail(ReasonMachinePin, reason, 0, 0)
	}
	if reason, violated := providerViolation(c, t); violated {
		return fail(ReasonProvider, reason, 0, 0)
	}
	if c.CPUMemGB > 0 && t.CPUMemGB > 0 && t.CPUMemGB < c.CPUMemGB {
		return fail(ReasonHostRAM, fmt.Sprintf("host RAM %dGB below required %dGB", t.CPUMemGB, c.CPUMemGB), t.CPUMemGB, c.CPUMemGB)
	}
	if c.CPUCores > 0 && t.CPUCores > 0 && t.CPUCores < c.CPUCores {
		return fail(ReasonCPUCores, fmt.Sprintf("host CPU cores %d below required %d", t.CPUCores, c.CPUCores), t.CPUCores, c.CPUCores)
	}
	if t.GPUNamingSignalsKnown && !InterconnectSatisfied(c.Interconnect, candidateDeviceSignals(t, c), nil, requiredGPUCount(c)) {
		req := strings.ToLower(strings.TrimSpace(c.Interconnect))
		return fail(ReasonInterconnect, fmt.Sprintf("interconnect %s required; host GPU naming shows no match", req), 0, 0)
	}

	if reason, ok := targetCompatibilityViolation(c, t); ok {
		return fail(reason.Kind, reason.Message, 0, 0)
	}
	if !c.HasGPURuntimeBounds() {
		return v
	}
	if !c.NeedsGPU() && !targetHasCUDACapableGPU(t) && !capConstraintAppliesToTarget(c, t) {
		return v
	}
	if len(t.Devices) == 0 {
		if !c.NeedsGPU() {
			return v
		}
		return fail(ReasonGPUClass, "no GPUs", 0, 0)
	}

	matching, classMatched, memMatched, maxMatched, minMatched, sawUnknownMax := matchingDeviceCount(t, c)
	required := requiredGPUCount(c)
	if matching < required {
		if matching > 0 {
			return fail(ReasonGPUCount,
				fmt.Sprintf("gpu count: %d matching GPU(s) < requested %d", matching, required),
				matching, required)
		}
		switch {
		case c.GPUClass != "" && !classMatched:
			return fail(ReasonGPUClass, fmt.Sprintf("no %s GPU", c.GPUClass), 0, required)
		case c.GPUMemGB > 0 && !memMatched:
			return fail(ReasonGPUMemory, fmt.Sprintf("no GPU with >=%dGB", c.GPUMemGB), 0, required)
		case c.MaxComputeCap != "" && !maxMatched:
			if sawUnknownMax {
				return fail(ReasonComputeCapMax, fmt.Sprintf("GPU compute capability unknown; excluded under max cap %s", c.MaxComputeCap), 0, required)
			}
			return fail(ReasonComputeCapMax, fmt.Sprintf("arch cap: no GPU with compute cap <= %s", c.MaxComputeCap), 0, required)
		case c.MinComputeCap != "" && !minMatched:
			return fail(ReasonComputeCapMin, fmt.Sprintf("arch floor: no GPU with compute cap >= %s", c.MinComputeCap), 0, required)
		default:
			return fail(ReasonGPUClass, "no GPU matching constraints", 0, required)
		}
	}

	if c.NeedsGPU() && t.ReservationsKnown {
		free, total, reserved := freeMatchingDeviceCount(t, c)
		if free < required {
			return fail(ReasonGPUAvailability,
				fmt.Sprintf("gpu availability: %d of %d matching GPU(s) free (%d reserved by active jobs), need %d", free, total, reserved, required),
				free, required)
		}
	}

	return v
}

// providerViolation rejects targets that cannot serve a requested rental
// provider. A provider request is a routing constraint no other provider's
// target — and no on-prem host, which has none — can satisfy. Like the
// machine pin, it is checked in the shared eligibility predicate so every
// system rejects it identically: the autopilot's on-prem passes guarded it,
// the launch prefilter did not, and a --provider job without the rental tag
// could be placed on-prem with the request silently ignored.
func providerViolation(c Constraints, t TargetSpec) (string, bool) {
	want := strings.TrimSpace(c.Provider)
	have := strings.TrimSpace(t.Provider)
	if want == "" || strings.EqualFold(have, want) {
		return "", false
	}
	where := "target has no rental provider"
	if have != "" {
		where = "target is " + t.Provider
	}
	return fmt.Sprintf("job requests provider %s; %s", c.Provider, where), true
}

// machinePinViolation rejects targets that are not a machine the job is
// pinned to. Pins name provider physical machines, so a target with no
// machine identity — every on-prem host — can never satisfy one; unknown
// fails closed for the same reason db.assertMachineAffinitySatisfied does.
func machinePinViolation(c Constraints, t TargetSpec) (string, bool) {
	if len(c.MachineAffinity) == 0 || db.MachineRefsMatch(c.MachineAffinity, t.MachineKey) {
		return "", false
	}
	where := "target has no provider machine identity"
	if t.MachineKey != "" {
		where = "target is " + t.MachineKey
	}
	return fmt.Sprintf("job is pinned to machine(s) %s; %s",
		strings.Join(c.MachineAffinity, ", "), where), true
}

func targetCompatibilityViolation(c Constraints, t TargetSpec) (EligibilityReason, bool) {
	reqs := c.VersionRequirements
	if len(reqs) == 0 {
		reqs = versionRequirementsFromConstraints(c)
	}
	if !cudaRuntimeCompatibilityAppliesToTarget(c, t) {
		reqs = filterCUDARequirements(reqs)
	}
	facts := compat.FactSet{}
	if t.CUDAVersion != "" {
		facts[compat.AxisCUDA] = t.CUDAVersion
	}
	if t.NVIDIADriverMajor > 0 {
		facts[compat.AxisNVIDIADriver] = targetDriverVersionFact(t)
	}
	if t.GLIBCXXVersion != "" {
		facts[compat.AxisGLIBCXX] = t.GLIBCXXVersion
	}
	reqs = filterDerivedDriverRequirementCoveredByCUDA(reqs, c, t)
	if violations := compat.Check(reqs, facts); len(violations) > 0 {
		return EligibilityReason{Kind: ReasonCompatibility, Message: compat.FormatViolation(violations[0])}, true
	}
	if cudaRuntimeCompatibilityAppliesToTarget(c, t) {
		if v := compat.ValidateCUDAChain(compat.CUDAChain{
			CUDAFloor:      strings.TrimSpace(c.MinCUDAVersion),
			DriverCUDA:     strings.TrimSpace(t.CUDAVersion),
			MinDriverMajor: c.MinDriverVersion,
			DriverMajor:    t.NVIDIADriverMajor,
		}); v != nil {
			return EligibilityReason{Kind: ReasonCUDAChain, Message: v.Message()}, true
		}
	}
	return EligibilityReason{}, false
}

func cudaRuntimeCompatibilityAppliesToTarget(c Constraints, t TargetSpec) bool {
	return c.NeedsGPU() || !targetConfirmedNoCUDACapableGPU(t)
}

func targetConfirmedNoCUDACapableGPU(t TargetSpec) bool {
	if targetHasCUDACapableGPU(t) || !t.GPUInventoryKnown {
		return false
	}
	for _, d := range t.Devices {
		if d.Family != "apple" {
			return false
		}
	}
	return true
}

func filterCUDARequirements(reqs []compat.Requirement) []compat.Requirement {
	filtered := make([]compat.Requirement, 0, len(reqs))
	for _, req := range reqs {
		if req.Axis == compat.AxisCUDA || req.Axis == compat.AxisNVIDIADriver {
			continue
		}
		filtered = append(filtered, req)
	}
	return filtered
}

func filterDerivedDriverRequirementCoveredByCUDA(reqs []compat.Requirement, c Constraints, t TargetSpec) []compat.Requirement {
	if !c.MinDriverVersionDerived || c.MinDriverVersion <= 0 || strings.TrimSpace(c.MinCUDAVersion) == "" ||
		t.NVIDIADriverMajor > 0 || strings.TrimSpace(t.CUDAVersion) == "" {
		return reqs
	}
	filtered := make([]compat.Requirement, 0, len(reqs))
	driverFloor := strconv.Itoa(c.MinDriverVersion)
	for _, req := range reqs {
		if req.Axis == compat.AxisNVIDIADriver && strings.TrimSpace(req.Value) == driverFloor {
			continue
		}
		filtered = append(filtered, req)
	}
	return filtered
}

// candidateDeviceSignals returns naming for the GPUs this job could actually
// be given: the devices satisfying its per-device constraints. Interconnect is
// judged on these rather than on the host as a whole, because a fabric between
// GPUs the job cannot use is not a fabric the job gets — on a host holding
// both SXM and PCIe cards, the SXM naming must not admit a job pinned to the
// PCIe ones.
//
// When no device matches, every device is returned: the job cannot run here
// anyway, and the GPU-class check reports that far more usefully than an
// interconnect rejection would.
func candidateDeviceSignals(t TargetSpec, c Constraints) []string {
	gc := ParseGPUConstraint(c.GPUClass)
	signals := make([]string, 0, len(t.Devices))
	for _, d := range t.Devices {
		if deviceSatisfiesPerDeviceConstraints(d, gc, c, t.MaxComputeCapUnknownFailsClosed) {
			signals = append(signals, d.Name+" "+d.Class)
		}
	}
	if len(signals) > 0 {
		return signals
	}
	for _, d := range t.Devices {
		signals = append(signals, d.Name+" "+d.Class)
	}
	return signals
}

func matchingDeviceCount(t TargetSpec, c Constraints) (matching int, classMatched bool, memMatched bool, maxMatched bool, minMatched bool, sawUnknownMax bool) {
	gc := ParseGPUConstraint(c.GPUClass)
	for _, d := range t.Devices {
		if c.GPUClass != "" && d.matchesGPUConstraint(gc) {
			classMatched = true
		}
		if c.GPUMemGB > 0 && d.MemoryGB >= c.GPUMemGB {
			memMatched = true
		}
		if c.MaxComputeCap != "" {
			if d.ComputeCap == "" {
				sawUnknownMax = true
				if !t.MaxComputeCapUnknownFailsClosed {
					maxMatched = true
				}
			} else if CompareComputeCap(d.ComputeCap, c.MaxComputeCap) <= 0 {
				maxMatched = true
			}
		}
		if c.MinComputeCap != "" && d.ComputeCap != "" && CompareComputeCap(d.ComputeCap, c.MinComputeCap) >= 0 {
			minMatched = true
		}
		if deviceSatisfiesPerDeviceConstraints(d, gc, c, t.MaxComputeCapUnknownFailsClosed) {
			matching += deviceCount(d)
		}
	}
	return matching, classMatched, memMatched, maxMatched, minMatched, sawUnknownMax
}

func deviceSatisfiesPerDeviceConstraints(d TargetDevice, gc GPUConstraint, c Constraints, unknownMaxFailsClosed bool) bool {
	if c.GPUClass != "" && !d.matchesGPUConstraint(gc) {
		return false
	}
	if c.GPUMemGB > 0 && d.MemoryGB < c.GPUMemGB {
		return false
	}
	if c.MaxComputeCap != "" {
		if d.ComputeCap == "" {
			return !unknownMaxFailsClosed
		}
		if CompareComputeCap(d.ComputeCap, c.MaxComputeCap) > 0 {
			return false
		}
	}
	if c.MinComputeCap != "" && (d.ComputeCap == "" || CompareComputeCap(d.ComputeCap, c.MinComputeCap) < 0) {
		return false
	}
	return true
}

func (d TargetDevice) matchesGPUConstraint(gc GPUConstraint) bool {
	if !gc.matchesSKUMemory(d.MemoryGB) {
		return false
	}
	if gc.mode == constraintFamily && d.Family == gc.normalized {
		return true
	}
	if gc.MatchesGPU(d.Class) || gc.MatchesGPUFullName(d.Name) {
		return true
	}
	for _, alias := range d.ClassAliases {
		if gc.MatchesGPU(alias) {
			return true
		}
	}
	for _, alias := range d.FullNameAlias {
		if gc.MatchesGPUFullName(alias) {
			return true
		}
	}
	if gc.mode == constraintExactGen || gc.mode == constraintMinGen {
		return gc.matchesGeneration(d.Generation)
	}
	return false
}

func freeMatchingDeviceCount(t TargetSpec, c Constraints) (free, matching, reserved int) {
	matchingIdx := make(map[int]struct{})
	anonMatching := 0
	gc := ParseGPUConstraint(c.GPUClass)
	for _, d := range t.Devices {
		if !deviceSatisfiesPerDeviceConstraints(d, gc, c, t.MaxComputeCapUnknownFailsClosed) {
			continue
		}
		if len(d.Indices) > 0 {
			for _, idx := range d.Indices {
				matchingIdx[idx] = struct{}{}
			}
		} else {
			anonMatching += deviceCount(d)
		}
	}
	matching = len(matchingIdx) + anonMatching
	if matching == 0 {
		return 0, 0, 0
	}

	claimedIdx := make(map[int]struct{})
	countReserved := 0
	for _, res := range t.Reservations {
		if c.SelfJobID != 0 && res.JobID == c.SelfJobID {
			continue
		}
		if len(res.Devices) > 0 {
			for _, idx := range res.Devices {
				if _, ok := matchingIdx[idx]; ok {
					claimedIdx[idx] = struct{}{}
				} else if anonMatching > 0 && !targetHasDeviceIndex(t, idx) {
					countReserved++
				}
			}
			continue
		}
		if reservationOverlapsTarget(t, res, c) {
			countReserved += res.Count
		}
	}
	reserved = len(claimedIdx) + countReserved
	if reserved > matching {
		reserved = matching
	}
	return matching - reserved, matching, reserved
}

func reservationOverlapsTarget(t TargetSpec, res GPUReservation, c Constraints) bool {
	rc := Constraints{GPUClass: res.GPUClass, GPUMemGB: res.GPUMemGB}
	gc := ParseGPUConstraint(c.GPUClass)
	rgc := ParseGPUConstraint(rc.GPUClass)
	for _, d := range t.Devices {
		if deviceSatisfiesPerDeviceConstraints(d, gc, c, t.MaxComputeCapUnknownFailsClosed) &&
			deviceSatisfiesPerDeviceConstraints(d, rgc, rc, t.MaxComputeCapUnknownFailsClosed) {
			return true
		}
	}
	return false
}

func targetHasDeviceIndex(t TargetSpec, idx int) bool {
	for _, d := range t.Devices {
		if slices.Contains(d.Indices, idx) {
			return true
		}
	}
	return false
}

func targetHasCUDACapableGPU(t TargetSpec) bool {
	if strings.TrimSpace(t.CUDAVersion) != "" || t.NVIDIADriverMajor > 0 {
		return true
	}
	for _, d := range t.Devices {
		if d.Family == "nvidia" || d.ComputeCap != "" {
			return true
		}
	}
	return false
}

func capConstraintAppliesToTarget(c Constraints, t TargetSpec) bool {
	if c.MinComputeCap == "" && c.MaxComputeCap == "" {
		return false
	}
	for _, d := range t.Devices {
		if d.Family != "apple" {
			return true
		}
	}
	return false
}

func targetDriverVersionFact(t TargetSpec) string {
	if version := strings.TrimSpace(t.NVIDIADriverVersion); version != "" {
		return version
	}
	return strconv.Itoa(t.NVIDIADriverMajor)
}

func canonicalTargetGPUClass(class, name string) string {
	for _, candidate := range []string{class, name} {
		norm := inventory.NormalizeGPUClass(candidate)
		if norm == "" {
			continue
		}
		if best := bestKnownGPUClassInFullName(norm); best != "" {
			return best
		}
		if generationOf(norm) != GenUnknown || ComputeCapForGPU(norm) != "" {
			return norm
		}
	}
	return inventory.NormalizeGPUClass(firstNonEmpty(class, name))
}

func familyForDevice(class, name string, gen GPUGeneration) string {
	switch {
	case gen.isNVIDIA():
		return "nvidia"
	case gen.isApple():
		return "apple"
	}
	for _, candidate := range []string{class, name} {
		norm := inventory.NormalizeGPUClass(candidate)
		if strings.Contains(norm, "nvidia") || strings.HasPrefix(norm, "rtx") ||
			strings.HasPrefix(norm, "gtx") || strings.HasPrefix(norm, "tesla") {
			return "nvidia"
		}
		if strings.HasPrefix(norm, "m1") || strings.HasPrefix(norm, "m2") ||
			strings.HasPrefix(norm, "m3") || strings.HasPrefix(norm, "m4") {
			return "apple"
		}
	}
	return ""
}

func requiredGPUCount(c Constraints) int {
	if c.NumGPUs > 0 {
		return c.NumGPUs
	}
	return 1
}

func deviceCount(d TargetDevice) int {
	if len(d.Indices) > 0 {
		return len(d.Indices)
	}
	if d.Count > 0 {
		return d.Count
	}
	return 1
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// KnownTargetGPUClasses returns normalized GPU class names known to the
// structured target matcher. Cloud constructors use this to reverse provider
// aliases into target-device aliases.
func KnownTargetGPUClasses() []string {
	classes := make([]string, 0, len(appleClassToGeneration)+len(gpucatalog.KnownNormalizedClasses()))
	for class := range appleClassToGeneration {
		classes = append(classes, class)
	}
	classes = append(classes, gpucatalog.KnownNormalizedClasses()...)
	return classes
}
