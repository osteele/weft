package compat

import "testing"

func TestNormalizePlatform(t *testing.T) {
	for raw, want := range map[string]string{
		"": "", " Linux/x86_64 ": "linux/amd64", "Darwin/aarch64": "darwin/arm64",
		"linux/arm64": "linux/arm64", "darwin/amd64": "darwin/amd64",
	} {
		got, err := NormalizePlatform(raw)
		if err != nil || got != want {
			t.Errorf("NormalizePlatform(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"amd64", "linux", "linux/", "/arm64", "linux/amd64/extra", "windows/amd64", "linux/mips", "linux /amd64"} {
		if _, err := NormalizePlatform(raw); err == nil {
			t.Errorf("accepted invalid platform %q", raw)
		}
	}
}
