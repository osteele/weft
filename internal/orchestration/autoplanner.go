package orchestration

import (
	"database/sql"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func buildAutoPlacementPlan(
	database *sql.DB,
	cfg *config.Config,
	jobs []*db.Job,
	reusable []campaign.InstanceCapacity,
) (campaign.AutoPlacementPlan, error) {
	if cfg == nil {
		var err error
		cfg, err = config.Load()
		if err != nil {
			return campaign.AutoPlacementPlan{}, err
		}
	}
	clients, err := BuildCloudClients(cfg)
	if err != nil {
		clients = nil
	}
	predCfg := buildPredictorConfig(cfg)
	overheadModel := buildOverheadModel(database)
	survivalModel := buildSurvivalModel(database)
	return campaign.BuildAutoPlacementPlan(
		database,
		cfg,
		clients,
		jobs,
		reusable,
		&predCfg,
		overheadModel,
		survivalModel,
		0,
	)
}
