package campaign

import (
	"math"
	"strconv"
	"strings"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/vastai"
)

// TargetSpecFromOffer normalizes a provider offer into placement's
// source-agnostic eligibility target.
func TargetSpecFromOffer(offer cloud.Offer) placement.TargetSpec {
	device := targetDeviceFromCloudGPU(offer.Provider, offer.GPUName, offer.GPUName, offer.GPUMemGB, offer.NumGPUs)
	return placement.TargetSpec{
		Name:                            offer.Key(),
		Provider:                        string(offer.Provider),
		Devices:                         []placement.TargetDevice{device},
		CUDAVersion:                     strings.TrimSpace(formatCloudCUDAVersion(offer.CUDAVersion)),
		MachineKey:                      offerMachineClaimKey(offer),
		MaxComputeCapUnknownFailsClosed: true,
	}
}

// TargetSpecFromCloudInstance normalizes a running/reusable rental into an
// eligibility target.
func TargetSpecFromCloudInstance(inst db.Launch) placement.TargetSpec {
	provider := cloud.Provider(strings.TrimSpace(inst.Provider))
	device := targetDeviceFromCloudGPU(provider, inst.GPUClass, inst.ResolvedGPUName, float64(inst.GPUMemGB), inst.NumGPUs)
	return placement.TargetSpec{
		Name:                            inst.ProviderInstanceID,
		Provider:                        inst.Provider,
		Devices:                         []placement.TargetDevice{device},
		CUDAVersion:                     strings.TrimSpace(formatCloudCUDAVersion(inst.CUDAVersion)),
		MachineKey:                      db.ProviderMachineKey(inst.Provider, inst.MachineID),
		MaxComputeCapUnknownFailsClosed: true,
	}
}

func targetDeviceFromCloudGPU(provider cloud.Provider, class, resolvedName string, memGB float64, count int) placement.TargetDevice {
	mem := 0
	if memGB > 0 {
		mem = int(math.Round(memGB))
	}
	device := placement.TargetDeviceFromGPU(class, resolvedName, mem, count, nil)
	if provider == cloud.ProviderVastai || provider == cloud.ProviderRunpod || device.Family == "" {
		device.Family = "nvidia"
	}
	for _, alias := range cloudGPUAliases(class, resolvedName) {
		device.AddClassAlias(alias)
	}
	return device
}

func cloudGPUAliases(class, resolvedName string) []string {
	var aliases []string
	add := func(alias string) {
		norm := inventory.NormalizeGPUClass(alias)
		if norm == "" {
			return
		}
		for _, existing := range aliases {
			if existing == norm {
				return
			}
		}
		aliases = append(aliases, norm)
	}
	for _, raw := range []string{class, resolvedName} {
		add(raw)
		for _, known := range placement.KnownTargetGPUClasses() {
			if vastai.GPUClassMatchesOfferName(known, raw) {
				add(known)
			}
		}
	}
	return aliases
}

func formatCloudCUDAVersion(v float64) string {
	if v <= 0 {
		return ""
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 2, 64), "0"), ".")
}
