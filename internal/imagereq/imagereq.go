package imagereq

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/osteele/weft/internal/cloud"
)

var (
	cudaReqRe   = regexp.MustCompile(`(?:^|[,\s])cuda>=([0-9]+(?:\.[0-9]+)?)`)
	driverReqRe = regexp.MustCompile(`(?:^|[,\s])driver>=([0-9]+)`)
	cacheMu     sync.Mutex
	cache       = map[string]cloud.ImageRequirements{}
)

// Resolve fetches OCI image config metadata and returns NVIDIA runtime
// requirements declared through NVIDIA_REQUIRE_CUDA.
func Resolve(ctx context.Context, image string, auth *cloud.RegistryAuth) (cloud.ImageRequirements, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return cloud.ImageRequirements{}, nil
	}
	ref, err := name.ParseReference(image)
	if err != nil {
		return cloud.ImageRequirements{}, fmt.Errorf("parse image reference %q: %w", image, err)
	}
	cacheKey := ref.Context().Name() + "@" + ref.Identifier()
	if auth != nil {
		cacheKey += "|" + auth.Host + "|" + auth.Username
	}
	cacheMu.Lock()
	if req, ok := cache[cacheKey]; ok {
		cacheMu.Unlock()
		return req, nil
	}
	cacheMu.Unlock()

	options := []remote.Option{remote.WithContext(ctx)}
	if auth != nil {
		options = append(options, remote.WithAuth(&authn.Basic{
			Username: auth.Username,
			Password: auth.Password,
		}))
	} else {
		options = append(options, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}
	img, err := remote.Image(ref, options...)
	if err != nil {
		return cloud.ImageRequirements{}, fmt.Errorf("fetch image config for %s: %w", image, err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return cloud.ImageRequirements{}, fmt.Errorf("read image config for %s: %w", image, err)
	}
	req := FromEnv(cfg.Config.Env)
	cacheMu.Lock()
	cache[cacheKey] = req
	cacheMu.Unlock()
	return req, nil
}

// FromEnv parses OCI config env values for NVIDIA_REQUIRE_CUDA constraints.
func FromEnv(env []string) cloud.ImageRequirements {
	var req cloud.ImageRequirements
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key != "NVIDIA_REQUIRE_CUDA" {
			continue
		}
		req = Merge(req, ParseNVIDIARequireCUDA(value))
	}
	return req
}

// ParseNVIDIARequireCUDA extracts the most restrictive minimum driver and CUDA
// floors from an NVIDIA_REQUIRE_CUDA expression.
func ParseNVIDIARequireCUDA(value string) cloud.ImageRequirements {
	var req cloud.ImageRequirements
	for _, match := range cudaReqRe.FindAllStringSubmatch(value, -1) {
		req.MinCUDAVersion = maxCUDAVersion(req.MinCUDAVersion, match[1])
	}
	for _, match := range driverReqRe.FindAllStringSubmatch(value, -1) {
		n, err := strconv.Atoi(match[1])
		if err == nil && n > 0 && (req.MinDriverVersion == 0 || n < req.MinDriverVersion) {
			req.MinDriverVersion = n
		}
	}
	return req
}

func Merge(a, b cloud.ImageRequirements) cloud.ImageRequirements {
	if b.MinDriverVersion > a.MinDriverVersion {
		a.MinDriverVersion = b.MinDriverVersion
	}
	a.MinCUDAVersion = maxCUDAVersion(a.MinCUDAVersion, b.MinCUDAVersion)
	return a
}

// cudaDriverFloors is the NVIDIA Linux x86_64 minimum driver version per CUDA
// toolkit version, from the CUDA toolkit release notes. Used to derive a
// driver floor when a CUDA floor is known but no image label supplied one.
// CUDA values are strings so component-wise compare orders future versions
// like "12.10" correctly (float parsing would treat "12.10" as 12.1).
var cudaDriverFloors = []struct {
	cuda   string
	driver int
}{
	{"12.0", 525},
	{"12.1", 530},
	{"12.2", 535},
	{"12.3", 545},
	{"12.4", 550},
	{"12.5", 555},
	{"12.6", 560},
	{"12.8", 570},
	{"12.9", 575},
	{"13.0", 580},
}

// MinDriverForCUDA returns the minimum NVIDIA Linux x86_64 driver version
// required for the given CUDA toolkit version (e.g. "12.8" → 570). For values
// between known tiers it returns the floor entry (e.g. "12.7" → 560 from the
// 12.6 row). Returns 0 when cuda is empty, unparsable, below the lowest
// tabled version (12.0), or STRICTLY ABOVE the highest tabled version (so a
// future CUDA toolkit doesn't silently get the highest-known driver, which
// would under-constrain placement).
func MinDriverForCUDA(cuda string) int {
	cuda = strings.TrimSpace(cuda)
	if cuda == "" {
		return 0
	}
	if !isCUDAVersion(cuda) {
		return 0
	}
	highest := cudaDriverFloors[len(cudaDriverFloors)-1].cuda
	if cmpCUDAVersion(cuda, highest) > 0 {
		// Above the highest tabled CUDA — refuse to guess. Caller (back-fill)
		// will leave MinDriverVersion at 0 and surface the unknown.
		return 0
	}
	driver := 0
	for _, row := range cudaDriverFloors {
		if cmpCUDAVersion(cuda, row.cuda) < 0 {
			break
		}
		driver = row.driver
	}
	return driver
}

// BackfillDriverFromCUDA returns req with MinDriverVersion raised to at least
// the value implied by MinCUDAVersion via [MinDriverForCUDA]. Takes the max
// of any existing MinDriverVersion and the CUDA-implied floor, so:
//   - An empty MinDriverVersion is populated (the original "back-fill" sense).
//   - A previously back-filled driver gets raised when MinCUDAVersion goes up
//     later in the pipeline (e.g. via an image-label merge in
//     ApplyImageMetadataRequirements).
//   - An explicit pin that already satisfies the CUDA floor is preserved.
//
// An explicit pin that does NOT satisfy the CUDA floor is raised. That's
// intentional FOR IMAGE-LABEL MERGES, where requirements only accumulate: an
// under-constrained driver against a known CUDA floor would pick offers the
// job cannot actually run on. User-explicit overrides that may LOWER the
// floor are composed one layer up (placement.RuntimeFloor.ApplyExplicit /
// FinalizeDriver), not here. Returns req unchanged when no CUDA floor is set
// or no driver mapping is known.
func BackfillDriverFromCUDA(req cloud.ImageRequirements) cloud.ImageRequirements {
	if req.MinCUDAVersion == "" {
		return req
	}
	d := MinDriverForCUDA(req.MinCUDAVersion)
	if d > req.MinDriverVersion {
		req.MinDriverVersion = d
	}
	return req
}

// cmpCUDAVersion compares two CUDA-style version strings ("12.8", "13.0",
// "12.10") component-wise as integers. Avoids the float-parse pitfall where
// "12.10" parses to 12.1.
func cmpCUDAVersion(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(as) {
			ai, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(bs[i])
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

// isCUDAVersion reports whether s parses as a CUDA-style "major.minor[.patch]"
// version (all components numeric).
func isCUDAVersion(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

func maxCUDAVersion(a, b string) string {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	// Component-wise integer compare so "12.10" > "12.8".
	if isCUDAVersion(a) && isCUDAVersion(b) {
		if cmpCUDAVersion(b, a) > 0 {
			return b
		}
		return a
	}
	// Fallback to string compare for malformed input.
	if b > a {
		return b
	}
	return a
}
