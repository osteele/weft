package dataloc

import (
	"regexp"
	"strconv"
	"strings"
)

// DepSpec is a parsed Python dependency declaration: package name plus the raw
// version specifier (e.g. ">=0.17", "==1.0", or "" when no version is given).
type DepSpec struct {
	Name string // canonical lowercase distribution name
	Spec string // PEP 440 version specifier portion, or "" if unpinned
}

// LibraryFloor declares a minimum CUDA toolkit version implied by depending on
// a Python distribution at or above MinVersion. Used only for libraries whose
// CUDA requirement is NOT already captured by their resolved torch wheel — for
// the common case, dataloc.ScanTorchPin handles it.
//
// Add entries only with a confirmed bug case and a release-notes citation.
type LibraryFloor struct {
	Name        string // canonical pypi name, lowercase
	MinVersion  string // inclusive lower bound that triggers the floor (PEP 440 version)
	CudaVersion string // required CUDA toolkit version (e.g. "12.8")
	NeedsDevel  bool   // true if the library JIT-compiles kernels and needs nvcc
	Reason      string // citation for maintainers
}

// libraryCudaFloors lists libraries with CUDA requirements that go beyond what
// their torch dependency would imply. Keep small and well-cited.
//
// LibraryMinCUDAFromDeps applies the highest CUDA across all entries that the
// dep spec admits, so multiple entries per library form a stepped floor as
// versions bump their CUDA requirement.
//
// Both vLLM entries set NeedsDevel because vLLM's flashinfer path JIT-compiles
// kernels at run time and needs nvcc: a pytorch/pytorch runtime image broke
// SGLang/vLLM pilots that nvidia/cuda:12.4.1-devel-ubuntu22.04 served.
var libraryCudaFloors = []LibraryFloor{
	{
		Name:        "vllm",
		MinVersion:  "0.17.0",
		CudaVersion: "12.8",
		NeedsDevel:  true,
		Reason:      "vLLM 0.17 integrates flash-attention 4 which requires CUDA 12.8 / driver 570",
	},
	{
		Name:        "vllm",
		MinVersion:  "0.20.0",
		CudaVersion: "13.0",
		NeedsDevel:  true,
		Reason:      "vLLM 0.20 switched default to CUDA 13.0 (PyTorch 2.11 upgrade, manylinux_2_28); needs driver 580+",
	},
}

// forEachMatchingLibraryFloor calls fn for every libraryCudaFloors entry that
// is matched by a dep in deps. The list is tiny so the callback is invoked for
// every match even if the caller could early-exit.
func forEachMatchingLibraryFloor(deps []DepSpec, fn func(floor LibraryFloor)) {
	for _, dep := range deps {
		name := strings.ToLower(strings.TrimSpace(dep.Name))
		if name == "" {
			continue
		}
		for _, floor := range libraryCudaFloors {
			if name != floor.Name {
				continue
			}
			if !specAdmitsMinVersion(dep.Spec, floor.MinVersion) {
				continue
			}
			fn(floor)
		}
	}
}

// LibraryMinCUDAFromDeps returns the highest CUDA toolkit version implied by
// any DepSpec that matches an entry in libraryCudaFloors. Returns "" when no
// applicable floor is found.
//
// A DepSpec matches a floor entry when (a) names match (case-insensitive), and
// (b) the spec admits at least one version ≥ the floor's MinVersion. An empty
// spec (bare package name) is conservatively treated as "could resolve to the
// latest" and matches; callers that want stricter behavior should filter first.
func LibraryMinCUDAFromDeps(deps []DepSpec) string {
	best := ""
	forEachMatchingLibraryFloor(deps, func(floor LibraryFloor) {
		if best == "" || cmpCUDAVersion(floor.CudaVersion, best) > 0 {
			best = floor.CudaVersion
		}
	})
	return best
}

// LibraryNeedsDevelFromDeps reports whether any DepSpec that matches a
// libraryCudaFloors entry has NeedsDevel set. This captures libraries whose
// wheels JIT-compile kernels at runtime and therefore require an image with
// nvcc (the -devel variant), not just the CUDA runtime.
func LibraryNeedsDevelFromDeps(deps []DepSpec) bool {
	needs := false
	forEachMatchingLibraryFloor(deps, func(floor LibraryFloor) {
		if floor.NeedsDevel {
			needs = true
		}
	})
	return needs
}

// specAdmitsMinVersion reports whether the version spec could resolve to a
// version ≥ minVersion. An empty or wildcard spec admits any version, including
// the latest, so it returns true.
//
// Recognised operators: ==, ===, !=, >=, <=, >, <, ~=. Compound specs
// separated by commas are AND-combined.
func specAdmitsMinVersion(spec, minVersion string) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return true
	}
	for _, part := range strings.Split(spec, ",") {
		if !partAdmitsMinVersion(strings.TrimSpace(part), minVersion) {
			return false
		}
	}
	return true
}

var specOpRe = regexp.MustCompile(`^(===|==|!=|<=|>=|~=|<|>)\s*(.+)$`)

func partAdmitsMinVersion(part, minVersion string) bool {
	if part == "" {
		return true
	}
	m := specOpRe.FindStringSubmatch(part)
	if m == nil {
		// Bare version like "0.17" — treat as ==. Matches the `==` branch
		// below: admits any version ≥ minVersion (e.g. bare "0.18" admits
		// minVersion "0.17.0" because 0.18 ≥ 0.17.0).
		return cmpPEP440(stripLocal(part), minVersion) >= 0
	}
	op, ver := m[1], stripLocal(m[2])
	cmp := cmpPEP440(ver, minVersion)
	switch op {
	case "==", "===":
		// "==0.17.0" admits 0.17.0; "==0.16" admits only 0.16, which is below 0.17.
		return cmp >= 0
	case "!=":
		// Excludes ver, but other versions could still be ≥ minVersion.
		return true
	case ">=", ">":
		// ≥0.17 admits 0.17+ → true. ≥0.16 also admits 0.17+ → true.
		// <0.17 case is handled by `<` below.
		return true
	case "<":
		// <0.18 still admits 0.17.x → true if ver > minVersion.
		return cmp > 0
	case "<=":
		// <=0.17 admits 0.17 → true if ver ≥ minVersion.
		return cmp >= 0
	case "~=":
		// PEP 440 compatible-release: `~=X.Y` is `[X.Y, (X+1).0)`, and
		// `~=X.Y.Z` is `[X.Y.Z, X.(Y+1).0)`. The admitted set intersects
		// [minVersion, ∞) iff the upper exclusive bound is > minVersion.
		upper := compatibleReleaseUpperBound(ver)
		if upper == "" {
			// Malformed (single-component version like ~=1) — over-apply
			// rather than under-apply.
			return true
		}
		return cmpPEP440(upper, minVersion) > 0
	}
	return true
}

// compatibleReleaseUpperBound computes the upper exclusive bound for a PEP 440
// compatible-release spec (`~=ver`). For `~=X.Y`, returns `(X+1)`. For
// `~=X.Y.Z` (and longer), returns `X.(Y+1)`. Returns "" when ver has fewer
// than two components or the bump component is non-numeric.
func compatibleReleaseUpperBound(ver string) string {
	parts := strings.Split(ver, ".")
	if len(parts) < 2 {
		return ""
	}
	bumpIdx := len(parts) - 2
	bumpVal, err := strconv.Atoi(parts[bumpIdx])
	if err != nil {
		return ""
	}
	upper := make([]string, bumpIdx+1)
	copy(upper, parts[:bumpIdx])
	upper[bumpIdx] = strconv.Itoa(bumpVal + 1)
	return strings.Join(upper, ".")
}

// stripLocal removes a PEP 440 local version segment ("+cu128") and any
// trailing whitespace, so version comparisons operate on the public release.
func stripLocal(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	return v
}

// cmpPEP440 returns -1/0/+1 for the numeric ordering of two release-segment
// version strings (e.g. "0.17", "0.17.0", "1.2.3"). Non-numeric components are
// compared lexicographically. Sufficient for the library-floor use case; not a
// full PEP 440 implementation.
func cmpPEP440(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		ai := ""
		bi := ""
		if i < len(as) {
			ai = as[i]
		}
		if i < len(bs) {
			bi = bs[i]
		}
		an, aerr := strconv.Atoi(ai)
		bn, berr := strconv.Atoi(bi)
		if aerr == nil && berr == nil {
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
			continue
		}
		// Missing components compare as 0 against a numeric peer.
		if ai == "" {
			ai = "0"
		}
		if bi == "" {
			bi = "0"
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}

// cmpCUDAVersion compares two CUDA toolkit version strings like "12.8", "13.0".
// Component-wise integer comparison so "12.10" correctly sorts after "12.8"
// (float parsing would treat "12.10" as 12.1).
func cmpCUDAVersion(a, b string) int {
	return cmpPEP440(strings.TrimSpace(a), strings.TrimSpace(b))
}

// ScanUVRunWith extracts inline dependencies from `--with`/`--with=...`
// arguments in a shell command, e.g.
//
//	uv run --with "vllm>=0.17" --with pynvml==12.0 python foo.py
//
// returns DepSpec{vllm, ">=0.17"} and DepSpec{pynvml, "==12.0"}.
//
// A single `--with` argument may carry multiple comma-separated specs (e.g.
// `--with "vllm>=0.17,pynvml>=12.0"`), which uv supports; each is parsed
// independently.
//
// Quotes around the value are stripped. Whitespace inside a `--with` value is
// not supported — a value with whitespace would be passed as a single shell
// token via quoting, which strings.Fields would preserve as one field
// including the quote characters. Most real commands use the operator forms
// covered here (`>=`, `==`, `~=`, etc.) without intervening whitespace.
func ScanUVRunWith(command string) []DepSpec {
	tokens := strings.Fields(command)
	var out []DepSpec
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		var value string
		switch {
		case tok == "--with":
			if i+1 >= len(tokens) {
				continue
			}
			i++
			value = tokens[i]
		case strings.HasPrefix(tok, "--with="):
			value = strings.TrimPrefix(tok, "--with=")
		case tok == "--with-requirements" || strings.HasPrefix(tok, "--with-requirements="):
			// Requirements file content isn't read here; skip safely. A
			// future enhancement could resolve the file relative to the
			// project root.
			if tok == "--with-requirements" && i+1 < len(tokens) {
				i++
			}
			continue
		default:
			continue
		}
		// Strip outer quotes once before splitting so quote characters don't
		// land in the middle of a sub-spec after the split.
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		for _, sub := range strings.Split(value, ",") {
			dep := parseDepSpec(sub)
			if dep.Name != "" {
				out = append(out, dep)
			}
		}
	}
	return out
}

// ParseDepSpecs parses PEP 508 / PEP 723–style dependency strings (e.g.
// `"vllm>=0.17"`, `"torch"`) into DepSpec values. Whitespace and trailing
// environment markers (`; python_version >= "3.10"`) are tolerated.
func ParseDepSpecs(deps []string) []DepSpec {
	out := make([]DepSpec, 0, len(deps))
	for _, d := range deps {
		if spec := parseDepSpec(d); spec.Name != "" {
			out = append(out, spec)
		}
	}
	return out
}

var depNameRe = regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.\-]*)`)

func parseDepSpec(raw string) DepSpec {
	v := strings.TrimSpace(raw)
	v = strings.Trim(v, `"'`)
	// Drop PEP 508 environment markers — they don't affect resolution here.
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	// Drop extras: "vllm[server]>=0.17" → "vllm>=0.17"
	if i := strings.IndexByte(v, '['); i >= 0 {
		if j := strings.IndexByte(v[i:], ']'); j >= 0 {
			v = v[:i] + strings.TrimSpace(v[i+j+1:])
		}
	}
	m := depNameRe.FindStringSubmatch(v)
	if m == nil {
		return DepSpec{}
	}
	name := strings.ToLower(m[1])
	spec := strings.TrimSpace(v[len(m[1]):])
	return DepSpec{Name: name, Spec: spec}
}
