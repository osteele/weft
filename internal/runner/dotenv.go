package runner

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotenv reads a .env file and returns key=value pairs.
// Skips comments (#) and blank lines. Does not handle quoting beyond
// trimming surrounding double/single quotes from values.
func LoadDotenv(path string) ([]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var result []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// Strip surrounding quotes
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		result = append(result, key+"="+val)
	}
	return result, scanner.Err()
}

// LoadDotenvFiles loads .env and .env.local from a directory.
// Returns combined key=value pairs; .env.local values override .env.
func LoadDotenvFiles(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}

	envVars, err := LoadDotenv(dir + "/.env")
	if err != nil {
		return nil, err
	}

	localVars, err := LoadDotenv(dir + "/.env.local")
	if err != nil {
		return nil, err
	}

	// Merge: local overrides base
	merged := make(map[string]string)
	for _, ev := range envVars {
		if k, v, ok := strings.Cut(ev, "="); ok {
			merged[k] = v
		}
	}
	for _, ev := range localVars {
		if k, v, ok := strings.Cut(ev, "="); ok {
			merged[k] = v
		}
	}

	result := make([]string, 0, len(merged))
	for k, v := range merged {
		result = append(result, k+"="+v)
	}
	return result, nil
}
