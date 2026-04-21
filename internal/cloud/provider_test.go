package cloud

import "testing"

func TestProviderDisplayName(t *testing.T) {
	cases := map[Provider]string{
		ProviderVastai:     "Vast.ai",
		ProviderRunpod:     "RunPod",
		"":                 "",
		Provider("gcp"):    "",
		Provider("VASTAI"): "", // case-sensitive: normalised upstream
	}
	for p, want := range cases {
		if got := p.DisplayName(); got != want {
			t.Errorf("Provider(%q).DisplayName() = %q, want %q", string(p), got, want)
		}
	}
}

func TestProviderShortCode(t *testing.T) {
	cases := map[Provider]string{
		ProviderVastai:  "va",
		ProviderRunpod:  "rp",
		"":              "",
		Provider("gcp"): "",
	}
	for p, want := range cases {
		if got := p.ShortCode(); got != want {
			t.Errorf("Provider(%q).ShortCode() = %q, want %q", string(p), got, want)
		}
	}
}
