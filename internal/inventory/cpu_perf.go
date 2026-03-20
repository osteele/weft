package inventory

import "strings"

// cpuPerfEntry maps a CPU model substring to a relative single-core performance
// factor. The baseline (1.0) is the AMD EPYC 7402. Factors are approximate,
// derived from published Geekbench 6 single-core scores.
//
// Update this table when `weft host discover` encounters an unknown CPU model
// (it will log a message). Look up the Geekbench 6 single-core score and divide
// by ~1200 (EPYC 7402 baseline).
type cpuPerfEntry struct {
	substr string  // case-insensitive substring to match in CPU model string
	factor float64 // relative single-core performance (1.0 = EPYC 7402)
}

// cpuPerfTable is ordered most-specific first. The first matching entry wins.
var cpuPerfTable = []cpuPerfEntry{
	// Apple Silicon
	{"Apple M4 Max", 3.6},
	{"Apple M4 Pro", 3.4},
	{"Apple M4", 3.3},
	{"Apple M3 Max", 3.1},
	{"Apple M3 Pro", 3.0},
	{"Apple M3", 2.9},
	{"Apple M2 Ultra", 2.7},
	{"Apple M2 Max", 2.5},
	{"Apple M2 Pro", 2.4},
	{"Apple M2", 2.3},
	{"Apple M1 Ultra", 2.4},
	{"Apple M1 Max", 2.4},
	{"Apple M1 Pro", 2.3},
	{"Apple M1", 2.2},

	// AMD EPYC (server)
	{"EPYC 9754", 1.4}, // Bergamo
	{"EPYC 9654", 1.5}, // Genoa
	{"EPYC 7763", 1.3}, // Milan
	{"EPYC 7713", 1.2}, // Milan
	{"EPYC 7542", 1.1}, // Rome
	{"EPYC 7402", 1.0}, // Rome (baseline)

	// Intel Xeon (server)
	{"Xeon w9-3595X", 1.8},
	{"Xeon w7-3465X", 1.5},
	{"Xeon Gold 6430", 1.3},
	{"Xeon Gold 6348", 1.1},
	{"Xeon Gold 6248", 0.9},
	{"Xeon Silver 4314", 0.9},
	{"Xeon Silver 4210R", 0.8},
	{"Xeon Silver 4210", 0.8},

	// AMD Ryzen (desktop/workstation)
	{"Ryzen 9 7950X", 2.7},
	{"Ryzen 9 5950X", 2.0},
	{"Ryzen 9 5900X", 1.9},
	{"Ryzen 7 5800X", 1.9},

	// Intel Core (desktop/workstation)
	{"Core i9-14900K", 2.9},
	{"Core i9-13900K", 2.8},
	{"Core i9-12900K", 2.4},
	{"Core i9-9900K", 1.5},
}

// LookupCPUFactor returns the cpu_factor for the given CPU model string,
// and whether it was found in the table. Returns (1.0, false) for unknown CPUs.
func LookupCPUFactor(cpuModel string) (factor float64, known bool) {
	normalized := normalizeCPUModel(cpuModel)
	for _, entry := range cpuPerfTable {
		if strings.Contains(normalized, strings.ToLower(entry.substr)) {
			return entry.factor, true
		}
	}
	return 1.0, false
}

// normalizeCPUModel strips trademark markers and extra whitespace for matching.
func normalizeCPUModel(model string) string {
	s := strings.ToLower(model)
	for _, marker := range []string{"(r)", "(tm)", "cpu", "@"} {
		s = strings.ReplaceAll(s, marker, "")
	}
	// Collapse runs of whitespace
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}
