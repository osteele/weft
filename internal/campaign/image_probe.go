package campaign

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
)

const ImageProbeTimeout = 20 * time.Second

// ValidateJobImageAvailability fetches the selected OCI image config for a
// rental-bound job. It catches missing images, bad credentials, and registry
// throttling before a provider starts burning rental attempts on pull failures.
func ValidateJobImageAvailability(ctx context.Context, cfg *config.Config, localDir, command string) error {
	image, _, pullSecret := ResolveJobImageSettings(localDir, command)
	image = strings.TrimSpace(image)
	if image == "" {
		return nil
	}
	auth, err := cfg.RegistryAuthForImage(image, pullSecret)
	if err != nil {
		return fmt.Errorf("container image %s registry auth: %w", image, err)
	}
	if _, err := imageRequirementResolver(ctx, image, auth); err != nil {
		return fmt.Errorf("container image %s is not pullable with configured auth: %w", image, err)
	}
	return nil
}
