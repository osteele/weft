// Package dataloc tracks what data assets exist on which hosts,
// enabling data-locality-aware job placement.
package dataloc

import (
	"strings"
	"time"
)

// AssetKind identifies the type of data asset.
type AssetKind string

const (
	AssetHFModel    AssetKind = "hf-model"
	AssetHFDataset  AssetKind = "hf-dataset"
	AssetCheckpoint AssetKind = "checkpoint"
	AssetJobOutput  AssetKind = "job-output"
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
			case "job-output":
				return DataAsset{Kind: AssetJobOutput, ID: id}, true
			default:
				return DataAsset{}, false
			}
		}
	}
	return DataAsset{}, false
}

// Ref returns the string reference form of a DataAsset (e.g., "hf:meta-llama/Llama-3-8B").
func (a DataAsset) Ref() string {
	switch a.Kind {
	case AssetHFModel:
		return "hf:" + a.ID
	case AssetHFDataset:
		return "hf-dataset:" + a.ID
	case AssetCheckpoint:
		return "checkpoint:" + a.ID
	case AssetJobOutput:
		return "job-output:" + a.ID
	default:
		return string(a.Kind) + ":" + a.ID
	}
}

// InputRef represents a parsed --input value, which is either a data asset
// reference (e.g., "hf:meta-llama/Llama-3-8B") or a local file path
// (e.g., "~/sources/vidur/data/").
type InputRef struct {
	// Asset is non-nil if the input is a data asset reference.
	Asset *DataAsset
	// FilePath is non-empty if the input is a local file path to sync.
	FilePath string
}

// IsAsset returns true if this input is a data asset reference.
func (r InputRef) IsAsset() bool { return r.Asset != nil }

// IsFilePath returns true if this input is a local file path.
func (r InputRef) IsFilePath() bool { return r.FilePath != "" }

// ParseInputRef parses an --input value into either a data asset reference or a
// file path. Asset refs have a recognized "prefix:" format (hf:, hf-dataset:,
// checkpoint:). Everything else is treated as a file path.
func ParseInputRef(s string) InputRef {
	if asset, ok := ParseAssetRef(s); ok {
		return InputRef{Asset: &asset}
	}
	// "local:" prefix marks a project-relative path; strip prefix and treat as file path.
	if strings.HasPrefix(s, "local:") {
		return InputRef{FilePath: s[len("local:"):]}
	}
	return InputRef{FilePath: s}
}

// ClassifyInputs splits a list of --input values into asset ref strings and
// file paths. Asset refs are returned as their original strings (for use with
// placement scoring). File paths are returned expanded for syncing.
func ClassifyInputs(inputs []string) (assetRefs []string, filePaths []string) {
	for _, s := range inputs {
		ref := ParseInputRef(s)
		if ref.IsAsset() {
			assetRefs = append(assetRefs, s)
		} else {
			filePaths = append(filePaths, ref.FilePath)
		}
	}
	return
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
	case AssetJobOutput:
		return "job-output:" + a.ID
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
