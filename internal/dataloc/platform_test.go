package dataloc

import "testing"

func TestScriptPlatformRequirement(t *testing.T) {
	meta, err := ParseScriptMeta("# /// script\n# [tool.weft]\n# platform = \"Linux/x86_64\"\n# ///\n")
	if err != nil || meta == nil || meta.Platform != "linux/amd64" {
		t.Fatalf("platform-only metadata = %+v, %v", meta, err)
	}
	for _, value := range []string{"42", "[]", "\"\"", "\"linux\"", "\"linux/unknown\""} {
		if _, err := ParseScriptMeta("# /// script\n# [tool.weft]\n# platform = " + value + "\n# ///\n"); err == nil {
			t.Errorf("invalid platform metadata %s silently accepted", value)
		}
	}
}
