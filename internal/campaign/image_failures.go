package campaign

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

const (
	imagePrestartFailureThreshold = 3
	imagePrestartFailureWindow    = 24 * time.Hour
)

// ImagePrestartFailureError blocks fresh launches for an image that has just
// failed before OnStart/agent readiness on several consecutive rentals.
type ImagePrestartFailureError struct {
	Image     string
	Count     int
	LaunchIDs []int64
}

func (e *ImagePrestartFailureError) Error() string {
	if e == nil {
		return ""
	}
	latest := ""
	if len(e.LaunchIDs) > 0 {
		latest = fmt.Sprintf(" latest=wi%d", e.LaunchIDs[0])
	}
	return fmt.Sprintf("container image %s has %d consecutive pre-start rental failures%s; blocking fresh launches for this image until a successful/agent-started launch breaks the chain or the recent-failure window (%s) elapses",
		e.Image, e.Count, latest, imagePrestartFailureWindowLabel())
}

func (e *ImagePrestartFailureError) Fingerprint() string {
	if e == nil {
		return ""
	}
	return "image/prestart-failure:" + strings.ToLower(strings.TrimSpace(e.Image))
}

func applyImagePrestartFailureBlocks(database *sql.DB, raw []GroupRawOffers) []GroupRawOffers {
	if database == nil || len(raw) == 0 {
		return raw
	}
	out := append([]GroupRawOffers(nil), raw...)
	cache := make(map[string]error)
	since := time.Now().Add(-imagePrestartFailureWindow).Unix()
	for i := range out {
		if out[i].Err != nil {
			continue
		}
		image := imagePrestartFailureKey(out[i].Group.Image)
		err, ok := cache[image]
		if !ok {
			chain, chainErr := db.RecentImagePrestartFailureChain(database, image, imagePrestartFailureThreshold, since)
			if chainErr != nil {
				err = fmt.Errorf("check image pre-start failure history for %s: %w", image, chainErr)
			} else if chain != nil {
				err = &ImagePrestartFailureError{
					Image:     image,
					Count:     chain.Count,
					LaunchIDs: append([]int64(nil), chain.LaunchIDs...),
				}
			}
			cache[image] = err
		}
		if err != nil {
			out[i].Err = err
			out[i].Offers = nil
		}
	}
	return out
}

func imagePrestartFailureKey(image string) string {
	image = normalizeRentalImageAlias(image)
	if strings.TrimSpace(image) == "" {
		return cloud.DefaultImage
	}
	return image
}

func imagePrestartFailureWindowLabel() string {
	if imagePrestartFailureWindow%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(imagePrestartFailureWindow/time.Hour))
	}
	return imagePrestartFailureWindow.String()
}
