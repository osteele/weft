package dataloc

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	fromPretrainedPattern      = regexp.MustCompile(`\.from_pretrained\(\s*["']([^"']+)["']`)
	snapshotDownloadPattern    = regexp.MustCompile(`snapshot_download\(\s*["']([^"']+)["']`)
	hfHubDownloadPattern       = regexp.MustCompile(`hf_hub_download\(\s*["']([^"']+)["']`)
	sentenceTransformerPattern = regexp.MustCompile(`SentenceTransformer\(\s*["']([^"']+)["']`)
	loadDatasetPattern         = regexp.MustCompile(`load_dataset\(\s*["']([^"']+)["']`)
	argparseDefaultPattern     = regexp.MustCompile(`default\s*=\s*["']([^"']+)["']`)
	modelFlagPattern           = regexp.MustCompile(`["']--(?:model|model-name|base-model)["']`)
)

var pyScanSkipDirs = map[string]struct{}{
	".git":          {},
	".jj":           {},
	".mypy_cache":   {},
	".pytest_cache": {},
	".ruff_cache":   {},
	".venv":         {},
	"__pycache__":   {},
	"build":         {},
	"dist":          {},
	"node_modules":  {},
	"venv":          {},
}

// ScanPythonHFRefs scans .py files in dir for HF model/dataset references.
// Returns deduplicated asset refs like "hf:gpt2", "hf-dataset:wikitext".
func ScanPythonHFRefs(dir string) []string {
	var refs []string
	seen := map[string]struct{}{}

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if _, skip := pyScanSkipDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".py" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		scanPythonContent(string(content), &refs, seen)
		return nil
	})

	return refs
}

func scanPythonContent(content string, refs *[]string, seen map[string]struct{}) {
	lines := strings.Split(content, "\n")
	nonCommentLines := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			nonCommentLines = append(nonCommentLines, "")
			continue
		}
		nonCommentLines = append(nonCommentLines, line)
	}
	content = strings.Join(nonCommentLines, "\n")

	addModelMatches(content, fromPretrainedPattern, refs, seen)
	addModelMatches(content, snapshotDownloadPattern, refs, seen)
	addModelMatches(content, hfHubDownloadPattern, refs, seen)
	addModelMatches(content, sentenceTransformerPattern, refs, seen)
	addDatasetMatches(content, loadDatasetPattern, refs, seen)

	for _, block := range findArgparseBlocks(lines) {
		if !looksLikeModelFlagLine(block) {
			continue
		}
		for _, m := range argparseDefaultPattern.FindAllStringSubmatch(block, -1) {
			id := strings.TrimSpace(m[1])
			if isLikelyHFModelArgDefault(id) {
				addRef(newAssetRef(AssetHFModel, id), refs, seen)
			}
		}
	}
}

func addModelMatches(content string, pattern *regexp.Regexp, refs *[]string, seen map[string]struct{}) {
	for _, m := range pattern.FindAllStringSubmatch(content, -1) {
		id := strings.TrimSpace(m[1])
		if isHFModelID(id) {
			addRef(newAssetRef(AssetHFModel, id), refs, seen)
		}
	}
}

func addDatasetMatches(content string, pattern *regexp.Regexp, refs *[]string, seen map[string]struct{}) {
	for _, m := range pattern.FindAllStringSubmatch(content, -1) {
		id := strings.TrimSpace(m[1])
		if isHFModelID(id) {
			addRef(newAssetRef(AssetHFDataset, id), refs, seen)
		}
	}
}

func looksLikeModelFlagLine(line string) bool {
	return modelFlagPattern.MatchString(line)
}

func findArgparseBlocks(lines []string) []string {
	var blocks []string

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "add_argument(") {
			continue
		}

		var blockLines []string
		depth := 0
		seenOpen := false

		for ; i < len(lines); i++ {
			current := lines[i]
			if strings.HasPrefix(strings.TrimSpace(current), "#") {
				continue
			}
			blockLines = append(blockLines, current)
			depth += strings.Count(current, "(")
			depth -= strings.Count(current, ")")
			if strings.Contains(current, "add_argument(") {
				seenOpen = true
			}
			if seenOpen && depth <= 0 {
				break
			}
		}

		if len(blockLines) > 0 {
			blocks = append(blocks, strings.Join(blockLines, "\n"))
		}
	}

	return blocks
}

func newAssetRef(kind AssetKind, id string) string {
	return DataAsset{Kind: kind, ID: id}.String()
}

func addRef(ref string, refs *[]string, seen map[string]struct{}) {
	if _, ok := seen[ref]; ok {
		return
	}
	seen[ref] = struct{}{}
	*refs = append(*refs, ref)
}

func isLikelyHFModelArgDefault(s string) bool {
	if !isHFModelID(s) {
		return false
	}
	if strings.Contains(s, "/") {
		return true
	}
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
		switch r {
		case '-', '_', '.':
			return true
		}
	}
	return false
}

// ScanCommandHFRefs extracts HF model IDs from a command string.
// Looks for --model/--model-name/--base-model args with org/model values.
func ScanCommandHFRefs(command string) []string {
	tokens := strings.Fields(command)
	var refs []string
	seen := map[string]struct{}{}

	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		var value string

		switch {
		case token == "--model" || token == "--model-name" || token == "--base-model":
			if i+1 < len(tokens) {
				i++
				value = tokens[i]
			}
		case strings.HasPrefix(token, "--model="):
			value = strings.TrimPrefix(token, "--model=")
		case strings.HasPrefix(token, "--model-name="):
			value = strings.TrimPrefix(token, "--model-name=")
		case strings.HasPrefix(token, "--base-model="):
			value = strings.TrimPrefix(token, "--base-model=")
		}

		value = strings.TrimSpace(strings.Trim(value, `"'`))
		value = strings.TrimRight(value, ",;")
		if value == "" {
			continue
		}
		if strings.Count(value, "/") != 1 {
			continue
		}
		if isHFModelID(value) {
			addRef(newAssetRef(AssetHFModel, value), &refs, seen)
		}
	}

	return refs
}

// isHFModelID returns true if s looks like a HuggingFace hub ID.
func isHFModelID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") {
		return false
	}
	if strings.Contains(s, `\`) {
		return false
	}
	if strings.Count(s, "/") > 1 {
		return false
	}
	lower := strings.ToLower(s)
	if strings.Contains(lower, "output/") ||
		strings.Contains(lower, "data/") ||
		strings.Contains(lower, "cache/") ||
		strings.Contains(lower, "checkpoint") {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '-', '_', '.', '/':
			continue
		default:
			return false
		}
	}
	return true
}
