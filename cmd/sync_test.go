package cmd

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestBase64EncodingPreservesSpecialCharacters(t *testing.T) {
	// Test that base64 encoding properly handles commands with shell-special characters
	// like parentheses which caused issues with nested shell quoting
	testCases := []struct {
		name    string
		command string
	}{
		{
			name:    "parentheses in codec names",
			command: `uv run compression-lab report --codecs "v2f-buckets(64),v2f-buckets(128)"`,
		},
		{
			name:    "single quotes",
			command: `echo 'hello world'`,
		},
		{
			name:    "double quotes",
			command: `echo "hello world"`,
		},
		{
			name:    "dollar signs",
			command: `echo $HOME && ls $TMPDIR`,
		},
		{
			name:    "backticks",
			command: "echo `date`",
		},
		{
			name:    "mixed special chars",
			command: `cd ~/dir && uv run script.py --opt="value(1)" --flag='test'`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			jobLine := "123\t/home/user\t" + tc.command + "\ttest description\t\t"

			// Encode and decode
			encoded := base64.StdEncoding.EncodeToString([]byte(jobLine))
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("failed to decode: %v", err)
			}

			if string(decoded) != jobLine {
				t.Errorf("roundtrip failed:\n  got:  %q\n  want: %q", string(decoded), jobLine)
			}

			// Verify encoded string has no shell-special characters
			shellSpecial := []string{"(", ")", "'", "\"", "$", "`", "\\", ";", "&", "|", "<", ">"}
			for _, char := range shellSpecial {
				if strings.Contains(encoded, char) {
					t.Errorf("encoded string contains shell-special character %q: %s", char, encoded)
				}
			}
		})
	}
}
