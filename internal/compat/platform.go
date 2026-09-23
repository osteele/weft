package compat

import (
	"fmt"
	"strings"
)

// NormalizePlatform validates a workload OS/architecture requirement. Empty
// means unconstrained; accepted aliases are the spellings reported by uname.
func NormalizePlatform(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return "", nil
	}
	os, arch, ok := strings.Cut(value, "/")
	if ok {
		switch arch {
		case "x86_64":
			arch = "amd64"
		case "aarch64":
			arch = "arm64"
		}
		if (os == "linux" || os == "darwin") && (arch == "amd64" || arch == "arm64") {
			return os + "/" + arch, nil
		}
	}
	return "", fmt.Errorf("invalid platform %q: expected linux/amd64, linux/arm64, darwin/amd64, or darwin/arm64", raw)
}
