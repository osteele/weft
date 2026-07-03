package compat

import (
	"fmt"
	"strconv"
	"strings"
)

// CUDAChain holds the resolved values of the end-to-end CUDA compatibility
// chain for one placed job. Empty / zero fields mean "unknown here" and skip
// their link — each enforcement point validates the links it has values for,
// with the SAME comparison semantics, instead of re-implementing ad hoc
// pairwise comparisons (spec: specs/campaign-lifecycle.allium contract
// CUDACompatibilityChain).
//
// Missing-value policy is deliberately the caller's job: on-prem placement
// fails closed on unrecorded host facts, the cloud offer filter routes
// unknown-compat providers through its own fallback, and the agent probe
// fails open when nvidia-smi is unavailable. This validator only decides
// whether KNOWN values are compatible.
type CUDAChain struct {
	// CUDAFloor is the job's required CUDA floor: the max-merge of the torch
	// wheel / library dependency floors, fetched image-label requirements
	// (NVIDIA_REQUIRE_CUDA), and explicit cuda-driver-min overrides.
	CUDAFloor string
	// ImageToolkit is the CUDA toolkit version of the resolved Docker image
	// tag (e.g. "12.4" from pytorch/pytorch:...-cuda12.4-...).
	ImageToolkit string
	// DriverCUDA is the maximum CUDA version the target's NVIDIA driver
	// reports supporting (nvidia-smi header on hosts, cuda_max_good on
	// cloud offers).
	DriverCUDA string
	// MinDriverMajor is the required NVIDIA driver-major floor (explicit or
	// backfilled from CUDAFloor via imagereq.MinDriverForCUDA).
	MinDriverMajor int
	// DriverMajor is the target's actual NVIDIA driver major version.
	DriverMajor int
}

// CUDAChainLink names which link of the chain a violation broke.
type CUDAChainLink string

const (
	// CUDAChainFloorVsDriverCUDA: the required CUDA floor exceeds the CUDA
	// version the target driver supports. The wb32/wb36 failure class —
	// torch loads, reports "driver too old", and silently falls back to CPU.
	CUDAChainFloorVsDriverCUDA CUDAChainLink = "cuda_floor_vs_driver_cuda"
	// CUDAChainFloorVsDriverMajor: the required driver-major floor exceeds
	// the target's driver major. Integer form of the same constraint, used
	// where only the driver major is known (agent bootstrap probe).
	CUDAChainFloorVsDriverMajor CUDAChainLink = "driver_major_floor"
	// CUDAChainImageVsDriverCUDA: the image's CUDA toolkit MAJOR exceeds the
	// driver's supported CUDA major. Within one major the image toolkit is
	// deliberately NOT compared against the floor or the driver: CUDA minor
	// version compatibility lets a newer same-major toolkit run on older
	// drivers, and torch wheels bundle their CUDA user-space libraries, so
	// an older image tag is not proof the job cannot run (wb30).
	CUDAChainImageVsDriverCUDA CUDAChainLink = "image_toolkit_vs_driver_cuda_major"
)

// CUDAChainViolation reports which link of the chain failed and with what
// values.
type CUDAChainViolation struct {
	Link     CUDAChainLink
	Required string
	Actual   string
}

// Message renders the violation for logs and placement reasons.
func (v *CUDAChainViolation) Message() string {
	switch v.Link {
	case CUDAChainFloorVsDriverCUDA:
		return fmt.Sprintf("CUDA floor: driver supports CUDA %s < required %s", v.Actual, v.Required)
	case CUDAChainFloorVsDriverMajor:
		return fmt.Sprintf("driver floor: NVIDIA driver major %s < required >=%s", v.Actual, v.Required)
	case CUDAChainImageVsDriverCUDA:
		return fmt.Sprintf("image toolkit: image CUDA %s needs a newer driver CUDA major than %s", v.Required, v.Actual)
	default:
		return fmt.Sprintf("CUDA chain violation %s: %s vs %s", v.Link, v.Required, v.Actual)
	}
}

// ValidateCUDAChain asserts the CUDA compatibility chain for the values the
// caller knows. Returns nil when every evaluable link is satisfied.
//
// Hard links (unknown values skip the link):
//  1. CUDAFloor <= DriverCUDA — the dependency/label/explicit CUDA floor must
//     not exceed what the target driver supports.
//  2. MinDriverMajor <= DriverMajor — driver-major form of the same floor.
//  3. major(ImageToolkit) <= major(DriverCUDA) — a cross-major image toolkit
//     (e.g. a CUDA 13 image on a driver whose max CUDA is 12.x) cannot run.
//
// Intentionally NOT asserted: ImageToolkit >= CUDAFloor. Wheels bundle CUDA
// user-space libraries and same-major minor-version compatibility applies,
// so an image tag older than the dependency floor is routinely fine (wb30:
// a CUDA 12.4 image was wrongly rejected under a 12.5 floor even though the
// driver satisfied the floor).
func ValidateCUDAChain(c CUDAChain) *CUDAChainViolation {
	floor := strings.TrimSpace(c.CUDAFloor)
	driverCUDA := strings.TrimSpace(c.DriverCUDA)
	if floor != "" && driverCUDA != "" && compareDottedVersion(driverCUDA, floor) < 0 {
		return &CUDAChainViolation{
			Link:     CUDAChainFloorVsDriverCUDA,
			Required: floor,
			Actual:   driverCUDA,
		}
	}
	if c.MinDriverMajor > 0 && c.DriverMajor > 0 && c.DriverMajor < c.MinDriverMajor {
		return &CUDAChainViolation{
			Link:     CUDAChainFloorVsDriverMajor,
			Required: strconv.Itoa(c.MinDriverMajor),
			Actual:   strconv.Itoa(c.DriverMajor),
		}
	}
	imageToolkit := strings.TrimSpace(c.ImageToolkit)
	if imageToolkit != "" && driverCUDA != "" {
		imageMajor, imageOK := versionMajor(imageToolkit)
		driverMajor, driverOK := versionMajor(driverCUDA)
		if imageOK && driverOK && imageMajor > driverMajor {
			return &CUDAChainViolation{
				Link:     CUDAChainImageVsDriverCUDA,
				Required: imageToolkit,
				Actual:   driverCUDA,
			}
		}
	}
	return nil
}

// versionMajor extracts the leading integer component of a dotted version.
func versionMajor(v string) (int, bool) {
	parts := splitVersion(v)
	if len(parts) == 0 {
		return 0, false
	}
	return parts[0], true
}
