package dataloc

import (
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

// uvLockTorchHeader matches the start of the [[package]] block for torch.
var (
	uvLockNameRe         = regexp.MustCompile(`^name\s*=\s*"([^"]+)"`)
	uvLockVersionRe      = regexp.MustCompile(`^version\s*=\s*"([^"]+)"`)
	uvLockWhlCudaRe      = regexp.MustCompile(`/whl/(cu\d+)\b`)
	cudaRuntimeVersionRe = regexp.MustCompile(`^(\d+)\.(\d+)\.`)
)

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
		cudaFromRunt   string
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
		case "nvidia-cuda-runtime-cu12":
			if cudaFromRunt == "" {
				if m := cudaRuntimeVersionRe.FindStringSubmatch(curVersion); m != nil {
					cudaFromRunt = "cu" + m[1] + m[2]
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
		if curName == "torch" || curName == "nvidia-cuda-runtime-cu12" {
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
	case cudaFromRunt != "":
		pin.CudaVariant = cudaFromRunt
	}
	return pin
}

// scanPyprojectTorchPin extracts a torch pin from pyproject.toml. It looks at
// [project.dependencies] and [tool.uv] index URLs. The version returned is a
// best-effort: an exact pin from "torch==X.Y.Z", or the lower bound of a
// "torch>=X.Y" constraint.
func scanPyprojectTorchPin(path string) *TorchPin {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	tree, err := toml.Load(string(data))
	if err != nil {
		return nil
	}

	version := torchVersionFromDeps(tree)
	if version == "" {
		return nil
	}

	cuda := ""
	if t := tree.Get("tool.uv"); t != nil {
		if ut, ok := t.(*toml.Tree); ok {
			cuda = cudaVariantFromUvTree(ut)
		}
	}
	return &TorchPin{Version: version, CudaVariant: cuda}
}

func torchVersionFromDeps(tree *toml.Tree) string {
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
					if version := extractTorchVersionString(s); version != "" {
						return version
					}
				}
			}
		case *toml.Tree:
			// poetry-style: { torch = "^2.4.1" }
			if raw := deps.Get("torch"); raw != nil {
				if s, ok := raw.(string); ok {
					if version := extractTorchVersionString("torch " + s); version != "" {
						return version
					}
				}
			}
		}
	}
	return ""
}

var (
	pep508TorchRe  = regexp.MustCompile(`^\s*torch(?:\[[^\]]*\])?\s*([<>=!~]=?[^,;]*)`)
	versionTokenRe = regexp.MustCompile(`(\d+(?:\.\d+){0,2})`)
)

// extractTorchVersionString parses a single requirement string. It handles
// both "torch==2.4.1" and the poetry/uv-style "torch ^2.4.1". The returned
// string is the bare version (no operator, no local +cu suffix).
func extractTorchVersionString(req string) string {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(req)), "torch") {
		return ""
	}
	m := pep508TorchRe.FindStringSubmatch(req)
	if m == nil {
		// Allow "torch ^2.4.1" or "torch 2.4.1" forms by stripping the leading
		// name token and re-extracting a version.
		fields := strings.Fields(req)
		if len(fields) >= 2 {
			if v := versionTokenRe.FindString(fields[1]); v != "" {
				return v
			}
		}
		return ""
	}
	return versionTokenRe.FindString(m[1])
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
