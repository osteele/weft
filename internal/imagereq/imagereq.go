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

func Explicit(minDriver, minCUDA string) (cloud.ImageRequirements, error) {
	var req cloud.ImageRequirements
	minDriver = strings.TrimSpace(minDriver)
	if minDriver != "" {
		n, err := strconv.Atoi(minDriver)
		if err != nil || n <= 0 {
			return req, fmt.Errorf("min-driver must be a positive integer, got %q", minDriver)
		}
		req.MinDriverVersion = n
	}
	minCUDA = strings.TrimSpace(minCUDA)
	if minCUDA != "" {
		if _, err := strconv.ParseFloat(minCUDA, 64); err != nil {
			return req, fmt.Errorf("min-cuda must be a CUDA major.minor version, got %q", minCUDA)
		}
		req.MinCUDAVersion = minCUDA
	}
	return req, nil
}

func maxCUDAVersion(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return strings.TrimSpace(b)
	}
	if strings.TrimSpace(b) == "" {
		return strings.TrimSpace(a)
	}
	af, aerr := strconv.ParseFloat(a, 64)
	bf, berr := strconv.ParseFloat(b, 64)
	if aerr != nil || berr != nil {
		if b > a {
			return b
		}
		return a
	}
	if bf > af {
		return b
	}
	return a
}
