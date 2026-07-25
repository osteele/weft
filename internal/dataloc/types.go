// Package dataloc tracks what data assets exist on which hosts,
// enabling data-locality-aware job placement.
package dataloc

import (
	"fmt"
	"regexp"
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
	AssetCorpus     AssetKind = "corpus"
	// AssetNamed is a content-addressed asset published via `weft data publish`
	// and resolved through the named_assets table. The asset bytes live in R2
	// under the assets/<name> key and can be staged onto any host with R2
	// access, regardless of which host originally produced or uploaded them.
	AssetNamed AssetKind = "named-asset"
)

// ContentType describes how asset bytes are materialized in R2.
type ContentType string

const (
	ContentTypeFile      ContentType = "file"
	ContentTypeDirectory ContentType = "directory"
)

// DataAsset represents a data asset that exists on one or more hosts.
type DataAsset struct {
	Kind AssetKind // e.g., "hf-model", "hf-dataset", "checkpoint"
	ID   string    // e.g., "meta-llama/Llama-3-8B", "wikitext"
}

// RedundantHFModelExcludeGlob is the fnmatch-style pattern passed to HF download
// tools (`hf download --exclude`, snapshot_download ignore_patterns) to skip
// native-checkpoint directories that transformers/vLLM never load — most
// notably Meta's `original/consolidated.*.pth` Llama weights. Skipping these
// avoids a redundant multi-GB download that has blown setup-time budgets. Only
// applied to HF *model* inputs (datasets keep their full snapshot).
const RedundantHFModelExcludeGlob = "original/*"

// isRedundantHFModelPath reports whether a repo-relative file path is a native
// checkpoint artifact skipped by default for HF model inputs. It mirrors
// RedundantHFModelExcludeGlob for local size estimation.
func isRedundantHFModelPath(path string) bool {
	return strings.HasPrefix(strings.TrimPrefix(path, "./"), "original/")
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
			case "corpus":
				return DataAsset{Kind: AssetCorpus, ID: id}, true
			case "asset":
				return DataAsset{Kind: AssetNamed, ID: id}, true
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
	case AssetCorpus:
		return "corpus:" + a.ID
	case AssetNamed:
		return "asset:" + a.ID
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
func (a DataAsset) String() string { return a.Ref() }

var llama31AliasPattern = regexp.MustCompile(`^llama[-_]?3\.1[-_]?([0-9]+)b(?:[-_]?(instruct))?$`)

// CanonicalHFModelAlias resolves short local names that look like common
// experiment aliases but are not Hugging Face repo IDs.
func CanonicalHFModelAlias(id string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(id))
	key = strings.TrimPrefix(key, "hf:")
	key = strings.ReplaceAll(key, "_", "-")
	if strings.Contains(key, "/") {
		return "", false
	}
	match := llama31AliasPattern.FindStringSubmatch(key)
	if match == nil {
		return "", false
	}
	model := "meta-llama/Llama-3.1-" + match[1] + "B"
	if match[2] == "instruct" {
		model += "-Instruct"
	}
	return model, true
}

// ValidateExplicitHFInputs rejects explicit HF model refs that cannot be
// staged as submitted. Best-effort refs came from source/command inference and
// are allowed to fail during non-fatal prewarm.
func ValidateExplicitHFInputs(inputs, bestEffortInputs []string) error {
	bestEffort := make(map[string]struct{}, len(bestEffortInputs))
	for _, input := range bestEffortInputs {
		bestEffort[input] = struct{}{}
	}
	for _, input := range inputs {
		if _, ok := bestEffort[input]; ok {
			continue
		}
		asset, ok := ParseAssetRef(input)
		if !ok || asset.Kind != AssetHFModel {
			continue
		}
		if canonical, ok := CanonicalHFModelAlias(asset.ID); ok {
			return fmt.Errorf("%s is a shorthand model alias, not a Hugging Face repo ID; use hf:%s", input, canonical)
		}
		if !IsHFModelID(asset.ID) {
			return fmt.Errorf("%s is not a valid Hugging Face model repo ID", input)
		}
	}
	return nil
}

// HostDataEntry records that a specific asset exists on a specific host.
type HostDataEntry struct {
	Host        string
	Asset       DataAsset
	Path        string      // Filesystem path on the host (may be empty for HF cache)
	SizeBytes   int64       // Size in bytes (0 if unknown), excluding *.incomplete files
	ContentHash string      // SHA256 hex for registered checkpoints/assets; empty if unknown
	ContentType ContentType // file or directory when ContentHash is set
	LastSeen    time.Time   // When this entry was last confirmed
}
