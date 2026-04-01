package agentdeploy

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// loadRepoEnvVars reads WEFT_* environment variables from .envrc or .env in
// the given directory. Only simple `export KEY=VALUE` and `KEY=VALUE` lines
// are parsed; shell expansions and conditionals are ignored.
func loadRepoEnvVars(root string) map[string]string {
	vars := parseEnvFile(filepath.Join(root, ".envrc"))
	if len(vars) == 0 {
		vars = parseEnvFile(filepath.Join(root, ".env"))
	}
	return vars
}

func parseEnvFile(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	vars := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimPrefix(line, "export ")
		if !strings.HasPrefix(line, "WEFT_") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := line[:eq]
		val := line[eq+1:]
		// Strip surrounding quotes
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		vars[key] = val
	}
	return vars
}

// mergeEnvVars returns os.Environ() with the given vars added. Existing
// environment variables take precedence (so direnv still wins when active).
func mergeEnvVars(extra map[string]string) []string {
	env := os.Environ()
	existing := make(map[string]bool, len(env))
	for _, e := range env {
		if eq := strings.IndexByte(e, '='); eq > 0 {
			existing[e[:eq]] = true
		}
	}
	for k, v := range extra {
		if !existing[k] {
			env = append(env, k+"="+v)
		}
	}
	return env
}
