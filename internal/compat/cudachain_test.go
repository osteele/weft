package compat

import "testing"

func TestValidateCUDAChain(t *testing.T) {
	cases := []struct {
		name     string
		chain    CUDAChain
		wantLink CUDAChainLink // "" = no violation
	}{
		{
			name: "passing chain",
			chain: CUDAChain{
				CUDAFloor:      "12.4",
				ImageToolkit:   "12.4",
				DriverCUDA:     "12.8",
				MinDriverMajor: 550,
				DriverMajor:    570,
			},
		},
		{
			// wb30 regression (over-strict): an image toolkit BELOW the CUDA
			// floor must NOT be a violation when the driver satisfies the
			// floor — wheels bundle their CUDA user-space libraries, so the
			// floor constrains the driver, not the image tag.
			name: "wb30: image toolkit below floor passes when driver satisfies floor",
			chain: CUDAChain{
				CUDAFloor:    "12.5",
				ImageToolkit: "12.4",
				DriverCUDA:   "12.8",
			},
		},
		{
			// wb32/wb36 regression (under-strict): a driver whose supported
			// CUDA is below the wheel floor must FAIL instead of silently
			// passing and letting torch fall back to CPU at runtime.
			name: "wb32/wb36: driver below wheel floor fails",
			chain: CUDAChain{
				CUDAFloor:  "12.8",
				DriverCUDA: "12.0",
			},
			wantLink: CUDAChainFloorVsDriverCUDA,
		},
		{
			name: "driver major below required major fails",
			chain: CUDAChain{
				MinDriverMajor: 570,
				DriverMajor:    550,
			},
			wantLink: CUDAChainFloorVsDriverMajor,
		},
		{
			name: "driver major at required major passes",
			chain: CUDAChain{
				MinDriverMajor: 570,
				DriverMajor:    570,
			},
		},
		{
			name: "cross-major image toolkit fails",
			chain: CUDAChain{
				ImageToolkit: "13.0",
				DriverCUDA:   "12.8",
			},
			wantLink: CUDAChainImageVsDriverCUDA,
		},
		{
			name: "same-major newer-minor image toolkit passes (minor-version compatibility)",
			chain: CUDAChain{
				ImageToolkit: "12.8",
				DriverCUDA:   "12.0",
			},
		},
		{
			name: "dotted compare orders 12.10 above 12.8",
			chain: CUDAChain{
				CUDAFloor:  "12.8",
				DriverCUDA: "12.10",
			},
		},
		{
			name:  "unknown values skip all links",
			chain: CUDAChain{},
		},
		{
			name: "floor without driver fact skips (missing policy is the caller's)",
			chain: CUDAChain{
				CUDAFloor:      "12.8",
				MinDriverMajor: 570,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ValidateCUDAChain(tc.chain)
			if tc.wantLink == "" {
				if v != nil {
					t.Fatalf("ValidateCUDAChain = %+v (%s), want nil", v, v.Message())
				}
				return
			}
			if v == nil {
				t.Fatalf("ValidateCUDAChain = nil, want violation on link %s", tc.wantLink)
			}
			if v.Link != tc.wantLink {
				t.Fatalf("violation link = %s, want %s (violation: %+v)", v.Link, tc.wantLink, v)
			}
			if v.Message() == "" {
				t.Fatal("violation message is empty")
			}
		})
	}
}
