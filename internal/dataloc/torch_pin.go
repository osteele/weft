package dataloc

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	toml "github.com/pelletier/go-toml"
)

// TorchPin describes the torch version pinned in a project, plus the inferred
// CUDA wheel variant ("cu118", "cu121", ..., "cu128"). Either field can be
// empty if it could not be determined.
type TorchPin struct {
	Version     string // e.g. "2.4.1"
	CudaVariant string // e.g. "cu121"; "" if unknown
}

// TorchRequirement describes the direct torch dependency declared in
// pyproject.toml. It is intentionally separate from TorchPin: a range like
// torch>=2.5 proves the project uses torch, but it is not a resolved runtime
// pin and must not drive placement compatibility as if it were one.
type TorchRequirement struct {
	Spec    string
	Version string
	Exact   bool
}

// ScanTorchPin returns the torch pin for a project rooted at or above dir.
// It prefers uv.lock when present — that is what gets installed — and falls
// back to pyproject.toml. Returns nil if torch is not pinned.
//
// The walk stops at the first directory containing uv.lock or pyproject.toml,
// or when it crosses a filesystem boundary / reaches the filesystem root.
func ScanTorchPin(dir string) *TorchPin {
	root, ok := findProjectRoot(dir)
	if !ok {
		return nil
	}
	if pin := scanUVLock(filepath.Join(root, "uv.lock")); pin != nil {
		return pin
	}
	return scanPyprojectTorchPin(filepath.Join(root, "pyproject.toml"))
}

func findProjectRoot(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		for _, marker := range []string{"uv.lock", "pyproject.toml"} {
			if _, err := os.Stat(filepath.Join(abs, marker)); err == nil {
				return abs, true
			}
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", false
		}
		abs = parent
	}
}

// ProjectRoot returns the nearest ancestor directory of dir containing
// uv.lock or pyproject.toml. Returns "" if not found. Exported for callers
// outside this package that need the same project-root semantics
// ScanTorchPin uses.
func ProjectRoot(dir string) string {
	root, ok := findProjectRoot(dir)
	if !ok {
		return ""
	}
	return root
}

// ProjectUsesTorch returns true when the project containing dir declares a
// torch-family dependency. It walks up to find pyproject.toml (the same
// root semantics as ScanTorchPin) and does a substring scan. The check is
// intentionally cheap and heuristic — false positives cost a torch
// preflight (~3s) and false negatives skip it. Returns false when no
// project root is found.
func ProjectUsesTorch(dir string) bool {
	root := ProjectRoot(dir)
	if root == "" {
		return false
	}
	for _, name := range []string{"pyproject.toml", "requirements.txt"} {
		path := filepath.Join(root, name)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		hit := scanFileForTorch(f)
		_ = f.Close()
		if hit {
			return true
		}
	}
	return false
}

// HasUVLock reports whether the project containing dir has a uv.lock at
// its root. Walks upward to match ScanTorchPin's semantics — calling
// hasUVLock on a subdir of a uv-managed project should still return true.
func HasUVLock(dir string) bool {
	root := ProjectRoot(dir)
	if root == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(root, "uv.lock"))
	return err == nil
}

// ScanPyprojectTorchRequirement returns the direct torch requirement from the
// nearest pyproject.toml, if present. Unlike ScanTorchPin, it does not consider
// uv.lock and it returns non-exact ranges so callers can reject unlocked cloud
// submissions before they resolve a different CUDA stack on the rental.
func ScanPyprojectTorchRequirement(dir string) *TorchRequirement {
	root := ProjectRoot(dir)
	if root == "" {
		return nil
	}
	return scanPyprojectTorchRequirement(filepath.Join(root, "pyproject.toml"))
}

func scanFileForTorch(f *os.File) bool {
	const maxLines = 500
	scanner := bufio.NewScanner(f)
	for i := 0; i < maxLines && scanner.Scan(); i++ {
		line := strings.ToLower(scanner.Text())
		if strings.Contains(line, "torch") || strings.Contains(line, "pytorch") {
			return true
		}
	}
	return false
}

// uvLockTorchHeader matches the start of the [[package]] block for torch.
var (
	uvLockNameRe         = regexp.MustCompile(`^name\s*=\s*"([^"]+)"`)
	uvLockVersionRe      = regexp.MustCompile(`^version\s*=\s*"([^"]+)"`)
	uvLockWhlCudaRe      = regexp.MustCompile(`/whl/(cu\d+)\b`)
	cudaRuntimeVersionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.`)
	// cudaRuntimePackageRe matches uv.lock package names that ship native CUDA
	// runtime libraries bundled with a PyTorch wheel (the nvidia-*-cu12 family,
	// e.g. nvidia-cusparse-cu12, nvidia-nvjitlink-cu12). Pure-Python nvidia
	// packages such as nvidia-ml-py have no -cu12 suffix and do not match.
	cudaRuntimePackageRe = regexp.MustCompile(`^nvidia-.+-cu12$`)
)

// extraReusablePackages names non-nvidia packages that a PyTorch base image
// also provides bundled with its torch, and which must therefore not be
// reinstalled from the project lockfile when reusing the image's torch.
var extraReusablePackages = map[string]bool{
	"triton":         true,
	"pytorch-triton": true,
}

// torchImageBundledPackages are the torch packages a PyTorch base image
// provides directly. They are always excluded when reusing the image's torch,
// independent of the project's uv.lock.
var torchImageBundledPackages = []string{"torch", "torchaudio", "torchvision"}

var cudaBearingNvidiaPackages = map[string]bool{
	"nvidia-cublas-cu12":       true,
	"nvidia-cuda-cupti-cu12":   true,
	"nvidia-cuda-nvrtc-cu12":   true,
	"nvidia-cuda-runtime-cu12": true,
	"nvidia-cufft-cu12":        true,
	"nvidia-curand-cu12":       true,
	"nvidia-cusolver-cu12":     true,
	"nvidia-cusparse-cu12":     true,
	"nvidia-nvjitlink-cu12":    true,
}

// CUDAVariantVersion converts a torch CUDA wheel tag such as "cu128" or
// "cu121" to a provider CUDA version floor such as "12.8" or "12.1".
func CUDAVariantVersion(cudaVariant string) string {
	cu := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(cudaVariant)), "cu")
	if len(cu) < 3 {
		return ""
	}
	major := cu[:len(cu)-1]
	minor := cu[len(cu)-1:]
	for _, r := range major + minor {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return major + "." + minor
}

// TorchMinCUDAVersion returns the provider CUDA compatibility floor implied by
// the project's pinned torch/CUDA packages.
func TorchMinCUDAVersion(dir string) string {
	pin := ScanTorchPin(dir)
	if pin == nil {
		return ""
	}
	return CUDAVariantVersion(pin.CudaVariant)
}

// CUDAFamilyFloor converts a torch CUDA wheel tag such as "cu128" to the
// CUDA *family* compatibility floor: "12.0" for cu12x, "11.0" for cu11x,
// "13.0" for cu13x. Pip wheels bundle the CUDA user-mode runtime and run on
// any same-major driver under CUDA minor-version compatibility, so the host
// floor implied by a pip-installed torch is the family base, not the wheel's
// exact toolkit version. (Exact toolkit floors remain appropriate for
// image-label-derived requirements, where the host's toolkit must match.)
func CUDAFamilyFloor(cudaVariant string) string {
	exact := CUDAVariantVersion(cudaVariant)
	if exact == "" {
		return ""
	}
	major, _, _ := strings.Cut(exact, ".")
	return major + ".0"
}

// CUDARuntimePackages returns the names of CUDA runtime packages pinned in the
// uv.lock at uvLockPath — the nvidia-*-cu12 family plus triton. These ship
// native libraries bundled with a PyTorch wheel; when a job reuses an
// image-provided torch they must come from the image, not the lockfile, or a
// CUDA-version skew can shadow the image's libraries and break `import torch`.
// Returns nil if the file is missing or contains no such packages.
func CUDARuntimePackages(uvLockPath string) []string {
	data, err := os.ReadFile(uvLockPath)
	if err != nil {
		return nil
	}
	var pkgs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		m := uvLockNameRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		name := m[1]
		if seen[name] {
			continue
		}
		if cudaRuntimePackageRe.MatchString(name) || extraReusablePackages[name] {
			seen[name] = true
			pkgs = append(pkgs, name)
		}
	}
	return pkgs
}

// ImageProvidedTorchPackages returns the full set of packages to exclude from
// `uv sync` when reusing a PyTorch base image's torch: the torch packages
// themselves, plus the CUDA runtime packages (nvidia-*-cu12, triton) pinned in
// the uv.lock at uvLockPath. The torch packages are always included even when
// the lockfile is absent.
func ImageProvidedTorchPackages(uvLockPath string) []string {
	pkgs := append([]string(nil), torchImageBundledPackages...)
	return append(pkgs, CUDARuntimePackages(uvLockPath)...)
}

// UVNoInstallPackageFlags renders a ` --no-install-package <name>` flag for
// each package, ready to append to a `uv sync` command. Returns "" for an
// empty list.
func UVNoInstallPackageFlags(pkgs []string) string {
	var b strings.Builder
	for _, pkg := range pkgs {
		b.WriteString(" --no-install-package ")
		b.WriteString(pkg)
	}
	return b.String()
}

// scanUVLock scans uv.lock for a torch pin. Returns nil if the file is missing
// or torch is not present.
func scanUVLock(path string) *TorchPin {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var (
		torchVersion   string
		cudaFromWheel  string
		cudaFromNvidia string
		curName        string
		curVersion     string
		inPackageBlock bool
	)

	flush := func() {
		switch curName {
		case "torch":
			if curVersion != "" && torchVersion == "" {
				torchVersion = curVersion
			}
		default:
			if cudaBearingNvidiaPackages[curName] {
				if m := cudaRuntimeVersionRe.FindStringSubmatch(curVersion); m != nil {
					cu := "cu" + m[1] + m[2]
					if cu > cudaFromNvidia {
						cudaFromNvidia = cu
					}
				}
			}
		}
		curName = ""
		curVersion = ""
	}

	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "[[package]]" {
			flush()
			inPackageBlock = true
			continue
		}
		if !inPackageBlock {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			// New section (e.g. [package.metadata]) — ignore but keep current
			// curName/curVersion until next [[package]] flush.
			continue
		}
		if m := uvLockNameRe.FindStringSubmatch(trimmed); m != nil {
			curName = m[1]
			continue
		}
		if curName == "torch" || cudaBearingNvidiaPackages[curName] {
			if m := uvLockVersionRe.FindStringSubmatch(trimmed); m != nil && curVersion == "" {
				curVersion = m[1]
			}
		}
		if curName == "torch" && cudaFromWheel == "" {
			if m := uvLockWhlCudaRe.FindStringSubmatch(trimmed); m != nil {
				cudaFromWheel = m[1]
			}
		}
	}
	flush()

	if torchVersion == "" {
		return nil
	}
	pin := &TorchPin{Version: torchVersion}
	switch {
	case cudaFromWheel != "":
		pin.CudaVariant = cudaFromWheel
	case cudaFromNvidia != "":
		pin.CudaVariant = cudaFromNvidia
	}
	return pin
}

// scanPyprojectTorchPin extracts an exact torch pin from pyproject.toml. It
// looks at [project.dependencies] and [tool.uv] index URLs. Non-exact ranges
// such as "torch>=2.5" are deliberately not pins: an unlocked resolver can
// choose a newer CUDA wheel than the lower bound implies.
func scanPyprojectTorchPin(path string) *TorchPin {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	tree, err := toml.Load(string(data))
	if err != nil {
		return nil
	}

	req := torchRequirementFromDeps(tree)
	if req == nil || !req.Exact || req.Version == "" {
		return nil
	}

	cuda := ""
	if t := tree.Get("tool.uv"); t != nil {
		if ut, ok := t.(*toml.Tree); ok {
			cuda = cudaVariantFromUvTree(ut)
		}
	}
	return &TorchPin{Version: req.Version, CudaVariant: cuda}
}

func scanPyprojectTorchRequirement(path string) *TorchRequirement {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	tree, err := toml.Load(string(data))
	if err != nil {
		return nil
	}
	return torchRequirementFromDeps(tree)
}

func torchRequirementFromDeps(tree *toml.Tree) *TorchRequirement {
	candidates := [][]string{
		{"project", "dependencies"},
		{"dependency-groups", "dev"},
		{"tool", "poetry", "dependencies"},
	}
	for _, path := range candidates {
		v := tree.GetPath(path)
		if v == nil {
			continue
		}
		switch deps := v.(type) {
		case []interface{}:
			for _, item := range deps {
				if s, ok := item.(string); ok {
					if req := parseTorchRequirementString(s); req != nil {
						return req
					}
				}
			}
		case *toml.Tree:
			// poetry-style: { torch = "^2.4.1" }
			if raw := deps.Get("torch"); raw != nil {
				if s, ok := raw.(string); ok {
					if req := parseTorchRequirementString("torch " + s); req != nil {
						return req
					}
				}
			}
		}
	}
	return nil
}

var (
	pep508TorchRe  = regexp.MustCompile(`^\s*torch(?:\[[^\]]*\])?\s*([<>=!~]=?[^,;]*)`)
	versionTokenRe = regexp.MustCompile(`(\d+(?:\.\d+){0,2})`)
)

// extractTorchVersionString parses a single requirement string. It handles
// both "torch==2.4.1" and the poetry/uv-style "torch ^2.4.1". The returned
// string is the bare version (no operator, no local +cu suffix).
func extractTorchVersionString(req string) string {
	parsed := parseTorchRequirementString(req)
	if parsed == nil {
		return ""
	}
	return parsed.Version
}

func parseTorchRequirementString(req string) *TorchRequirement {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(req)), "torch") {
		return nil
	}
	m := pep508TorchRe.FindStringSubmatch(req)
	if m == nil {
		// Allow "torch ^2.4.1" or "torch 2.4.1" forms by stripping the leading
		// name token and re-extracting a version.
		fields := strings.Fields(req)
		if len(fields) >= 2 {
			if v := versionTokenRe.FindString(fields[1]); v != "" {
				return &TorchRequirement{
					Spec:    fields[1],
					Version: v,
					Exact:   !strings.HasPrefix(fields[1], "^") && !strings.HasPrefix(fields[1], "~"),
				}
			}
		}
		return &TorchRequirement{Spec: strings.TrimSpace(req)}
	}
	spec := strings.TrimSpace(m[1])
	return &TorchRequirement{
		Spec:    spec,
		Version: versionTokenRe.FindString(spec),
		Exact:   strings.HasPrefix(spec, "==") && !strings.HasPrefix(spec, "==="),
	}
}

// cudaVariantFromUvTree returns the cuXXX tag implied by [tool.uv] settings,
// either via an index/extra-index URL or a [tool.uv.sources] entry.
func cudaVariantFromUvTree(ut *toml.Tree) string {
	check := func(s string) string {
		if m := uvLockWhlCudaRe.FindStringSubmatch(s); m != nil {
			return m[1]
		}
		return ""
	}
	if v, ok := ut.Get("index-url").(string); ok {
		if cu := check(v); cu != "" {
			return cu
		}
	}
	if extras, ok := ut.Get("extra-index-url").([]interface{}); ok {
		for _, e := range extras {
			if s, ok := e.(string); ok {
				if cu := check(s); cu != "" {
					return cu
				}
			}
		}
	}
	if v, ok := ut.Get("extra-index-url").(string); ok {
		if cu := check(v); cu != "" {
			return cu
		}
	}
	return ""
}
