package opsqueue

import "testing"

// A runner built before producer-artifact support advertises the capability
// string and omits the version field. Reading that as "supports everything the
// capability ever covered" is what makes a stale agent accept a need it cannot
// satisfy, so the absent field must mean named-assets-only.
func TestSupportsArtifactNeedVersion(t *testing.T) {
	for _, tc := range []struct {
		name          string
		caps          []string
		version       int
		wantNamed     bool
		wantProducers bool
	}{
		{"no capability", nil, 0, false, false},
		{"legacy agent: capability, no version field", []string{CapabilityArtifactNeedV1}, 0, true, false},
		{"explicit v1", []string{CapabilityArtifactNeedV1}, ArtifactNeedVersionNamedAssets, true, false},
		{"corrected agent", []string{CapabilityArtifactNeedV1}, ArtifactNeedVersionProducerArtifacts, true, true},
		{"version without capability", nil, ArtifactNeedVersionProducerArtifacts, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &RunnerState{Capabilities: tc.caps, ArtifactNeedVersion: tc.version}
			if got := s.SupportsArtifactNeedVersion(ArtifactNeedVersionNamedAssets); got != tc.wantNamed {
				t.Errorf("named assets = %v, want %v", got, tc.wantNamed)
			}
			if got := s.SupportsArtifactNeedVersion(ArtifactNeedVersionProducerArtifacts); got != tc.wantProducers {
				t.Errorf("producer artifacts = %v, want %v", got, tc.wantProducers)
			}
		})
	}
	if (*RunnerState)(nil).SupportsArtifactNeedVersion(ArtifactNeedVersionNamedAssets) {
		t.Error("nil state must not claim support")
	}
}
