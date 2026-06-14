package dataloc

import (
	"bufio"
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
		if IsHFModelID(id) {
			addRef(newAssetRef(AssetHFModel, id), refs, seen)
		}
	}
}

func addDatasetMatches(content string, pattern *regexp.Regexp, refs *[]string, seen map[string]struct{}) {
	for _, m := range pattern.FindAllStringSubmatch(content, -1) {
		id := strings.TrimSpace(m[1])
		if IsHFModelID(id) {
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
	if !IsHFModelID(s) {
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
		if IsHFModelID(value) {
			addRef(newAssetRef(AssetHFModel, value), &refs, seen)
		}
	}

	return refs
}

// IsHFModelID returns true if s looks like a HuggingFace hub ID
// (e.g., "gpt2", "meta-llama/Llama-3-8B"). It rejects MIME types,
// file paths, and other strings that are unlikely to be valid HF repo IDs.
func IsHFModelID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if isLikelyMIMEType(s) {
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
	if i := strings.Index(s, "/"); i >= 0 {
		// HF Hub requires org/user names to be at least 2 characters.
		// Rejects math-notation false positives like "N/M", "A/B".
		if i < 2 {
			return false
		}
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

func isLikelyMIMEType(s string) bool {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), "/")
	if len(parts) != 2 {
		return false
	}

	switch parts[0] {
	case "application", "audio", "font", "image", "message", "model", "multipart", "text", "video":
	default:
		return false
	}

	for _, r := range parts[1] {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '.', '-', '+':
			continue
		default:
			return false
		}
	}

	return true
}

var (
	importPattern     = regexp.MustCompile(`^import\s+(\w+)`)
	fromImportPattern = regexp.MustCompile(`^from\s+([\w.]+)\s+import`)
)

// ScanPythonHFRefsForCommand scans only the Python scripts referenced in command
// (and their local imports) for HF model/dataset references. Falls back to empty
// if no .py files are found in the command.
func ScanPythonHFRefsForCommand(dir, command string) []string {
	scripts := ExtractPythonScriptsInDir(dir, command)
	if len(scripts) == 0 {
		return nil
	}

	var files []string
	seen := map[string]struct{}{}
	for _, script := range scripts {
		abs := resolveScriptPath(dir, command, script)
		if _, err := os.Stat(abs); err != nil {
			continue
		}
		if _, ok := seen[abs]; !ok {
			seen[abs] = struct{}{}
			files = append(files, abs)
		}
		for _, imp := range findLocalImports(abs, dir) {
			if _, ok := seen[imp]; !ok {
				seen[imp] = struct{}{}
				files = append(files, imp)
			}
		}
	}

	var refs []string
	refSeen := map[string]struct{}{}
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		scanPythonContent(string(content), &refs, refSeen)
	}
	return refs
}

// ExtractPythonScripts returns .py file paths from a shell command string.
// Handles patterns like "python foo.py", "uv run python -u foo.py", etc.
func ExtractPythonScripts(command string) []string {
	tokens := strings.Fields(command)
	var scripts []string
	for _, tok := range tokens {
		tok = strings.Trim(tok, `"'`)
		if strings.HasSuffix(tok, ".py") && !strings.Contains(tok, "=") {
			scripts = append(scripts, tok)
		}
	}
	return scripts
}

// ExtractPythonScriptsInDir returns Python script file paths referenced by a
// shell command, including local modules invoked with `python -m module`.
func ExtractPythonScriptsInDir(dir, command string) []string {
	tokens := strings.Fields(command)
	scripts := ExtractPythonScripts(command)
	seen := make(map[string]struct{}, len(scripts))
	for _, script := range scripts {
		seen[script] = struct{}{}
	}

	for i, tok := range tokens {
		if !isPythonExecToken(strings.Trim(tok, `"'`)) {
			continue
		}
		module := pythonModuleArg(tokens[i+1:])
		if module == "" {
			continue
		}
		script := resolveLocalPythonModule(dir, module)
		if script == "" {
			continue
		}
		if _, ok := seen[script]; ok {
			continue
		}
		seen[script] = struct{}{}
		scripts = append(scripts, script)
	}
	return scripts
}

// commandLeadingCD returns the directory named by a leading `cd <dir>` segment
// in a shell command (the common `cd sub/dir && ...` form), or "" when the
// command does not begin with a cd. Only a single literal directory token is
// recognized — no shell expansion, globbing, variable substitution, or quoted
// paths containing spaces.
func commandLeadingCD(command string) string {
	s := strings.TrimSpace(command)
	if s != "cd" && !strings.HasPrefix(s, "cd ") {
		return ""
	}
	end := len(s)
	for _, sep := range []string{"&&", ";", "|", "\n"} {
		if idx := strings.Index(s, sep); idx >= 0 && idx < end {
			end = idx
		}
	}
	fields := strings.Fields(s[:end])
	if len(fields) < 2 {
		return ""
	}
	target := strings.Trim(fields[1], `"'`)
	if target == "" || target == "-" || strings.ContainsAny(target, "$*?~`") {
		return ""
	}
	return target
}

// resolveScriptPath resolves a (possibly relative) script token from a shell
// command to a path. Relative tokens are resolved against the job working dir
// and — when the command begins with a literal `cd <dir> &&` — against that
// directory too, so a script launched like `cd sub && uv run train.py` is
// found. Returns the first candidate that exists on disk, else the working-dir
// candidate (preserving prior behavior when nothing matches).
func resolveScriptPath(dir, command, script string) string {
	if filepath.IsAbs(script) {
		return script
	}
	primary := filepath.Join(dir, script)
	candidates := []string{primary}
	if cd := commandLeadingCD(command); cd != "" {
		base := cd
		if !filepath.IsAbs(base) {
			base = filepath.Join(dir, cd)
		}
		candidates = append(candidates, filepath.Join(base, script))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return primary
}

func pythonModuleArg(tokens []string) string {
	for i, tok := range tokens {
		tok = strings.Trim(tok, `"'`)
		if tok == "-m" {
			if i+1 >= len(tokens) {
				return ""
			}
			return strings.Trim(tokens[i+1], `"'`)
		}
		if !strings.HasPrefix(tok, "-") {
			return ""
		}
	}
	return ""
}

func resolveLocalPythonModule(dir, module string) string {
	if dir == "" || module == "" || strings.HasPrefix(module, "-") || strings.Contains(module, string(filepath.Separator)) {
		return ""
	}
	relBase := strings.ReplaceAll(module, ".", string(filepath.Separator))
	for _, rel := range []string{relBase + ".py", filepath.Join(relBase, "__main__.py")} {
		abs := filepath.Join(dir, rel)
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			return rel
		}
	}
	return ""
}

// findLocalImports parses a Python file for import statements and resolves them
// to .py files under projectDir. Only returns files that exist on disk.
func findLocalImports(filePath, projectDir string) []string {
	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	defer f.Close()

	var imports []string
	seen := map[string]struct{}{}
	scriptDir := filepath.Dir(filePath)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		var modulePath string
		if m := fromImportPattern.FindStringSubmatch(line); m != nil {
			modulePath = m[1]
		} else if m := importPattern.FindStringSubmatch(line); m != nil {
			modulePath = m[1]
		} else {
			continue
		}

		// Convert dotted module path to file path
		relPath := strings.ReplaceAll(modulePath, ".", string(filepath.Separator)) + ".py"

		// Try relative to the script's directory first
		candidate := filepath.Join(scriptDir, relPath)
		if _, err := os.Stat(candidate); err == nil {
			if _, ok := seen[candidate]; !ok {
				seen[candidate] = struct{}{}
				imports = append(imports, candidate)
			}
			continue
		}

		// Try relative to project dir
		candidate = filepath.Join(projectDir, relPath)
		if _, err := os.Stat(candidate); err == nil {
			if _, ok := seen[candidate]; !ok {
				seen[candidate] = struct{}{}
				imports = append(imports, candidate)
			}
		}
	}
	return imports
}
