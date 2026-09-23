package dataloc

import (
	"strconv"
	"strings"
)

const MaxComputeCapAny = "any"

var archNameMaxComputeCap = map[string]string{
	"volta":       "7.0",
	"turing":      "7.5",
	"ampere":      "8.6",
	"ada":         "8.9",
	"adalovelace": "8.9",
	"hopper":      "9.0",
	"blackwell":   "12.0",
}

func TorchMaxComputeCap(version, cudaVariant string) string {
	maj, min, ok := parseTorchMajMin(version)
	if !ok {
		return ""
	}
	cu := strings.ToLower(strings.TrimSpace(cudaVariant))
	if cu == "cpu" {
		return ""
	}
	if cu == "" {
		cu = defaultTorchCudaVariant(maj, min)
		if cu == "" {
			return ""
		}
	}
	switch {
	case maj < 2:
		return "8.0"
	case maj == 2 && min <= 4:
		return "9.0"
	case maj == 2 && min == 5:
		if cu == "cu126" || cu == "cu128" {
			return "10.0"
		}
		return "9.0"
	case maj == 2 && min == 6:
		// The CUDA wheel variant names the bundled toolkit, not the highest
		// architecture compiled into the wheel. Torch 2.6 wheels, including
		// cu126 and cu128, contain kernels only through Hopper (sm_90).
		return "9.0"
	case maj == 2 && min >= 7:
		if cu == "cu126" || cu == "cu128" {
			return "12.0"
		}
		return "10.0"
	case maj >= 3:
		return "12.0"
	}
	return ""
}

func TorchMinComputeCap(version, cudaVariant string) string {
	maj, min, ok := parseTorchMajMin(version)
	if !ok {
		return ""
	}
	cu := strings.ToLower(strings.TrimSpace(cudaVariant))
	if cu == "cpu" {
		return ""
	}
	if cu == "" {
		cu = defaultTorchCudaVariant(maj, min)
		if cu == "" {
			return ""
		}
	}
	if cudaVariantNumber(cu) >= 128 {
		return "7.5"
	}
	return ""
}

func ResolveTorchMaxComputeCapForPersistence(archMax, dir string) string {
	norm := strings.ToLower(strings.TrimSpace(archMax))
	switch {
	case norm == "any":
		return MaxComputeCapAny
	case norm != "":
		if _, ok := parseComputeCap(norm); ok {
			return norm
		}
		if cap := archNameMaxComputeCap[normalizeArchName(norm)]; cap != "" {
			return cap
		}
		return ""
	}
	pin := ScanTorchPin(dir)
	if pin == nil {
		return MaxComputeCapAny
	}
	return TorchMaxComputeCap(pin.Version, pin.CudaVariant)
}

// ResolveJobTorchMaxComputeCapForPersistence resolves the persisted cap
// encoding (see ResolveTorchMaxComputeCapForPersistence) for a job command.
// Precedence: an explicit script [tool.weft] gpu-arch-max; then the torch
// release the PEP 723 script's own requirement determines (an exact pin or an
// upper bound), because `uv run <script>` imports torch from the script
// environment rather than the project lock (wb166); then the project torch
// pin, which also covers scripts with an open torch range.
func ResolveJobTorchMaxComputeCapForPersistence(dir, command string) string {
	archMax := ""
	if meta, err := ScanScriptMeta(dir, command); err == nil && meta != nil {
		archMax = meta.GPUArchMax
	}
	if strings.TrimSpace(archMax) == "" {
		if pin := ScanScriptTorchPin(dir, command); pin != nil {
			return TorchMaxComputeCap(pin.Version, pin.CudaVariant)
		}
	}
	return ResolveTorchMaxComputeCapForPersistence(archMax, dir)
}

func TorchMinComputeCapForDir(dir string) string {
	pin := ScanTorchPin(dir)
	if pin == nil {
		return ""
	}
	return TorchMinComputeCap(pin.Version, pin.CudaVariant)
}

func cudaVariantNumber(cu string) int {
	cu = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cu)), "cu")
	n, err := strconv.Atoi(cu)
	if err != nil {
		return 0
	}
	return n
}

func normalizeArchName(s string) string {
	return strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(s)))
}

func parseTorchMajMin(version string) (maj, min int, ok bool) {
	v := strings.TrimSpace(version)
	if v == "" {
		return 0, 0, false
	}
	if idx := strings.Index(v, "+"); idx >= 0 {
		v = v[:idx]
	}
	for _, sep := range []string{"a", "b", "rc", ".dev", "-"} {
		if idx := strings.Index(v, sep); idx >= 0 {
			v = v[:idx]
		}
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	majN, err1 := strconv.Atoi(parts[0])
	minN, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return majN, minN, true
}

func defaultTorchCudaVariant(maj, min int) string {
	if maj != 2 {
		return ""
	}
	switch min {
	case 0, 1:
		return "cu118"
	case 2, 3:
		return "cu121"
	case 4, 5, 6:
		return "cu124"
	case 7:
		return "cu126"
	case 8:
		return "cu128"
	}
	if min > 8 {
		return "cu128"
	}
	return ""
}

func parseComputeCap(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
