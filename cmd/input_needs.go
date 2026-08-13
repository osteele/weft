package cmd

import (
	"slices"

	"github.com/osteele/weft/internal/dataloc"
)

// appendNamedAssetInputNeeds maintains the persisted invariant that every
// named asset input is also a need. Inputs participate in placement; needs
// drive staging onto the selected execution target.
func appendNamedAssetInputNeeds(inputs, needs []string) []string {
	result := slices.Clone(needs)
	for _, input := range inputs {
		ref := dataloc.ParseInputRef(input)
		if !ref.IsAsset() || ref.Asset.Kind != dataloc.AssetNamed {
			continue
		}
		need := ref.Asset.Ref()
		if !slices.Contains(result, need) {
			result = append(result, need)
		}
	}
	return result
}

// reconcileEditedNamedAssetNeeds removes needs mirrored from the prior input
// set, then mirrors the replacement input set. Non-asset needs are preserved.
func reconcileEditedNamedAssetNeeds(previousInputs, inputs, needs []string) []string {
	previousMirrors := appendNamedAssetInputNeeds(previousInputs, nil)
	filtered := make([]string, 0, len(needs))
	for _, need := range needs {
		if !slices.Contains(previousMirrors, need) {
			filtered = append(filtered, need)
		}
	}
	return appendNamedAssetInputNeeds(inputs, filtered)
}
