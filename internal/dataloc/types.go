// Package dataloc tracks what data assets exist on which hosts,
// enabling data-locality-aware job placement.
package dataloc

import "time"

// AssetKind identifies the type of data asset.
type AssetKind string

const (
	AssetHFModel    AssetKind = "hf-model"
	AssetHFDataset  AssetKind = "hf-dataset"
	AssetCheckpoint AssetKind = "checkpoint"
)

// DataAsset represents a data asset that exists on one or more hosts.
type DataAsset struct {
	Kind AssetKind // e.g., "hf-model", "hf-dataset", "checkpoint"
	ID   string    // e.g., "meta-llama/Llama-3-8B", "wikitext"
}

// ParseAssetRef parses a string like "hf:meta-llama/Llama-3-8B" into a DataAsset.
// Format: "<kind-prefix>:<id>"
// Supported prefixes: "hf" (HuggingFace model), "hf-dataset", "checkpoint"
func ParseAssetRef(ref string) (DataAsset, bool) {
	for i := range len(ref) {
		if ref[i] == ':' {
			prefix := ref[:i]
			id := ref[i+1:]
			if id == "" {
				return DataAsset{}, false
			}
			switch prefix {
			case "hf":
				return DataAsset{Kind: AssetHFModel, ID: id}, true
			case "hf-dataset":
				return DataAsset{Kind: AssetHFDataset, ID: id}, true
			case "checkpoint":
				return DataAsset{Kind: AssetCheckpoint, ID: id}, true
			default:
				return DataAsset{}, false
			}
		}
	}
	return DataAsset{}, false
}

// String returns the canonical ref string for a DataAsset.
func (a DataAsset) String() string {
	switch a.Kind {
	case AssetHFModel:
		return "hf:" + a.ID
	case AssetHFDataset:
		return "hf-dataset:" + a.ID
	case AssetCheckpoint:
		return "checkpoint:" + a.ID
	default:
		return string(a.Kind) + ":" + a.ID
	}
}

// HostDataEntry records that a specific asset exists on a specific host.
type HostDataEntry struct {
	Host      string
	Asset     DataAsset
	Path      string    // Filesystem path on the host (may be empty for HF cache)
	SizeBytes int64     // Size in bytes (0 if unknown)
	LastSeen  time.Time // When this entry was last confirmed
}
