//go:build go1.27 && !go1.28 && goexperiment.simd && amd64

package simd

import "testing"

func TestAMD64ScannerSelectionRequiresProvenWidth(t *testing.T) {
	cases := []struct {
		name     string
		features CPUFeatures
		want     uint8
	}{
		{"scalar", 0, scanLevelScalar},
		{"avx2", CPUFeatureAVX2.mask(), scanLevelAVX2},
		{"avx512 alone", CPUFeatureAVX512.mask(), scanLevelScalar},
		{"avx512 cpu uses avx2", CPUFeatureAVX2.mask() | CPUFeatureAVX512.mask(), scanLevelAVX2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectAMD64ScannerLevel(tc.features); got != tc.want {
				t.Fatalf("selectAMD64ScannerLevel(%v) = %d, want %d", tc.features, got, tc.want)
			}
		})
	}
}
