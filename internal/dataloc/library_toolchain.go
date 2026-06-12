package dataloc

import "strings"

// ToolchainFloor declares a native userland requirement implied by a Python
// dependency whose runtime downloads or loads binaries outside wheel metadata.
type ToolchainFloor struct {
	Name           string
	MinVersion     string
	GLIBCXXVersion string
	Reason         string
}

// libraryToolchainFloors lists confirmed non-CUDA native runtime requirements.
// Keep this table small; prefer entries tied to a concrete failure mode.
var libraryToolchainFloors = []ToolchainFloor{
	{
		Name:           "pysr",
		MinVersion:     "0",
		GLIBCXXVersion: "3.4.30",
		Reason:         "PySR loads Julia through juliacall/juliapkg; official Julia 1.12 binaries require GLIBCXX_3.4.30",
	},
	{
		Name:           "juliacall",
		MinVersion:     "0",
		GLIBCXXVersion: "3.4.30",
		Reason:         "juliacall/juliapkg can download official Julia 1.12 binaries, which require GLIBCXX_3.4.30",
	},
}

// LibraryToolchainFloorFromDeps returns the highest native toolchain floor
// implied by dependency declarations.
func LibraryToolchainFloorFromDeps(deps []DepSpec) ToolchainFloor {
	var best ToolchainFloor
	for _, dep := range deps {
		name := strings.ToLower(strings.TrimSpace(dep.Name))
		if name == "" {
			continue
		}
		for _, floor := range libraryToolchainFloors {
			if name != floor.Name {
				continue
			}
			if !specAdmitsMinVersion(dep.Spec, floor.MinVersion) {
				continue
			}
			if floor.GLIBCXXVersion != "" && (best.GLIBCXXVersion == "" || cmpPEP440(floor.GLIBCXXVersion, best.GLIBCXXVersion) > 0) {
				best = floor
			}
		}
	}
	return best
}
