package campaign

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestApplyImagePrestartFailureBlocks_BlocksImageAlias(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().Unix()
	for i := 0; i < imagePrestartFailureThreshold; i++ {
		insertCampaignImageLaunchFixture(t, database, sglangRuntimeImage, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, "no agent activity observed", now-int64(30-i*5), 0, 0)
	}
	raw := []GroupRawOffers{{
		Group:  InstanceGroup{Image: legacySGLangRuntimeImage},
		Offers: []cloud.Offer{{ProviderID: "offer-1"}},
	}}

	got := applyImagePrestartFailureBlocks(database, raw)

	var imageErr *ImagePrestartFailureError
	if !errors.As(got[0].Err, &imageErr) {
		t.Fatalf("Err = %v, want ImagePrestartFailureError", got[0].Err)
	}
	if imageErr.Image != sglangRuntimeImage {
		t.Fatalf("Image = %q, want normalized public image %q", imageErr.Image, sglangRuntimeImage)
	}
	if got := imageErr.Error(); !strings.Contains(got, "recent-failure window (24h) elapses") {
		t.Fatalf("Error() = %q, want recent-failure window expiry", got)
	}
	if len(got[0].Offers) != 0 {
		t.Fatalf("Offers = %+v, want nil/empty after image block", got[0].Offers)
	}
}

func TestApplyImagePrestartFailureBlocks_AllowsAfterStartedLaunch(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().Unix()
	for i := 0; i < imagePrestartFailureThreshold; i++ {
		insertCampaignImageLaunchFixture(t, database, sglangRuntimeImage, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, "no agent activity observed", now-int64(60-i*5), 0, 0)
	}
	insertCampaignImageLaunchFixture(t, database, sglangRuntimeImage, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, "agent reached ready before failure", now-5, now-10, 0)
	raw := []GroupRawOffers{{
		Group:  InstanceGroup{Image: sglangRuntimeImage},
		Offers: []cloud.Offer{{ProviderID: "offer-1"}},
	}}

	got := applyImagePrestartFailureBlocks(database, raw)

	if got[0].Err != nil {
		t.Fatalf("Err = %v, want nil after started launch breaks chain", got[0].Err)
	}
	if len(got[0].Offers) != 1 || got[0].Offers[0].ProviderID != "offer-1" {
		t.Fatalf("Offers = %+v, want original offer preserved", got[0].Offers)
	}
}

func TestApplyImagePrestartFailureBlocks_UsesDefaultImageForEmptyGroupImage(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().Unix()
	for i := 0; i < imagePrestartFailureThreshold; i++ {
		insertCampaignImageLaunchFixture(t, database, cloud.DefaultImage, db.LaunchStatusFailed, db.TerminationReasonInfraFailure, "no agent activity observed", now-int64(30-i*5), 0, 0)
	}
	raw := []GroupRawOffers{{
		Group:  InstanceGroup{},
		Offers: []cloud.Offer{{ProviderID: "offer-1"}},
	}}

	got := applyImagePrestartFailureBlocks(database, raw)

	var imageErr *ImagePrestartFailureError
	if !errors.As(got[0].Err, &imageErr) {
		t.Fatalf("Err = %v, want ImagePrestartFailureError for default image", got[0].Err)
	}
	if imageErr.Image != cloud.DefaultImage {
		t.Fatalf("Image = %q, want default image %q", imageErr.Image, cloud.DefaultImage)
	}
}

func insertCampaignImageLaunchFixture(t *testing.T, database *sql.DB, image, status, reason, detail string, endedAt, agentReadyAt, onStartSeenAt int64) int64 {
	t.Helper()
	id, err := db.CreateLaunch(database, &db.Launch{
		Status:      db.LaunchStatusLaunching,
		Provider:    "vastai",
		DockerImage: image,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE launches
		    SET status = ?, ended_at = ?, termination_reason = ?, termination_detail = ?,
		        agent_ready_at_unix = ?, first_onstart_probe_seen_unix = ?
		  WHERE id = ?`,
		status, endedAt, reason, detail, agentReadyAt, onStartSeenAt, id,
	); err != nil {
		t.Fatalf("update launch fixture: %v", err)
	}
	return id
}
