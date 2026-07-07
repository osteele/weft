package campaign

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/osteele/weft/internal/config"
)

const ImageProbeTimeout = 20 * time.Second

// ValidateJobImageAvailability fetches the selected OCI image config for a
// rental-bound job. It catches missing images, bad credentials, and registry
// throttling before a provider starts burning rental attempts on pull failures.
func ValidateJobImageAvailability(ctx context.Context, cfg *config.Config, localDir, command string) error {
	image, _, pullSecret := ResolveJobImageSettings(localDir, command)
	image = normalizeRentalImageAlias(image)
	image = strings.TrimSpace(image)
	if image == "" {
		return nil
	}
	auth, err := cfg.RegistryAuthForImage(image, pullSecret)
	if err != nil {
		return fmt.Errorf("container image %s registry auth: %w", image, err)
	}
	if _, err := imageRequirementResolver(ctx, image, auth); err != nil {
		if imageProbeFailureIsTransient(err) {
			slog.Warn("container image availability probe failed; allowing submission",
				"component", "campaign",
				"image", image,
				"error", err)
			return nil
		}
		return fmt.Errorf("container image %s is not pullable with configured auth: %w", image, err)
	}
	return nil
}

func imageProbeFailureIsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var registryErr *transport.Error
	if errors.As(err, &registryErr) {
		if registryErr.Temporary() {
			return true
		}
		return registryErr.StatusCode == http.StatusTooManyRequests ||
			registryErr.StatusCode == http.StatusRequestTimeout ||
			registryErr.StatusCode >= http.StatusInternalServerError
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}
