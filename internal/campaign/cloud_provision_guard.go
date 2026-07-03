package campaign

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
)

// validateGroupCloudProvisionable rejects a cloud launch when a job in the
// group declares an --input asset weft has no way to provision onto a rental
// instance.
//
// checkpoint: and corpus: inputs are on-host named assets: they are registered
// against specific hosts via `weft data add` or corpus scans, and exist only as
// paths on those hosts. There is no Hub to download them from and no R2 upload
// path, so the bootstrap script and the agent prewarm path (which handle hf: /
// hf-dataset: inputs) cannot stage them. Launching such a job on a rental burns
// instance time and then fails inside the job with a missing-file error.
//
// Rejecting here keeps the job queued for an on-prem host that holds the asset
// — which is what a checkpoint:/corpus: input is for: data-locality placement.
// hf:/hf-dataset: inputs download from the Hub; local file-path inputs ride the
// source tarball; job-output: inputs are co-located or staged from R2 via the
// --needs path, so none of those are rejected.
func validateGroupCloudProvisionable(ctx context.Context, group InstanceGroup, database *sql.DB, client *r2.Client) error {
	for _, job := range group.Jobs {
		if job == nil {
			continue
		}
		for _, raw := range job.Inputs {
			asset, ok := dataloc.ParseAssetRef(raw)
			if !ok {
				continue
			}
			switch asset.Kind {
			case dataloc.AssetCheckpoint:
				info, err := resolveCheckpointTransport(ctx, database, client, asset)
				if err != nil {
					return fmt.Errorf(
						"job %s declares input %q, but weft could not verify whether the checkpoint is staged in R2 for rental use: %w",
						ids.FormatJobID(job.ID), raw, err)
				}
				if info.state == checkpointTransportR2Resident {
					continue
				}
				if info.state == checkpointTransportUnknown {
					return fmt.Errorf(
						"job %s declares input %q, but weft could not verify whether the checkpoint is staged in R2 for rental use; rental placement fails closed while transportability is unknown",
						ids.FormatJobID(job.ID), raw)
				}
				return fmt.Errorf(
					"job %s declares input %q, which weft cannot provision onto a cloud rental instance: "+
						"the checkpoint is registered on an inventory host but is not confirmed in R2. "+
						"Run this job on a host that holds the asset (see `weft data where %s`), "+
						"or run `weft data publish checkpoint:%s --name <name>` to stage it through the asset store",
					ids.FormatJobID(job.ID), raw, raw, asset.ID)
			case dataloc.AssetCorpus:
				return fmt.Errorf(
					"job %s declares input %q, which weft cannot provision onto a cloud rental instance: "+
						"%s assets exist only on hosts where they were registered. "+
						"Run this job on a host that holds the asset (see `weft data where %s`), "+
						"or publish the data to the Hugging Face Hub and declare it as an hf: / hf-dataset: input",
					ids.FormatJobID(job.ID), raw, asset.Kind, raw)
			}
		}
	}
	return nil
}
