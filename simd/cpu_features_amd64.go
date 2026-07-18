//go:build go1.27 && goexperiment.simd && amd64

package simd

import "simd/archsimd"

// detectX86CPUFeatures snapshots the x86 capabilities exposed by Go's SIMD
// runtime. It is called only during package initialization; hot scanner calls
// use the implementation selected by initStringScanner.
func detectX86CPUFeatures() CPUFeatures {
	var features CPUFeatures
	probes := [...]struct {
		feature CPUFeature
		has     bool
	}{
		{CPUFeatureAVX, archsimd.X86.AVX()},
		{CPUFeatureAVX2, archsimd.X86.AVX2()},
		{CPUFeatureAVX512, archsimd.X86.AVX512()},
		{CPUFeatureAVX512BITALG, archsimd.X86.AVX512BITALG()},
		{CPUFeatureAVX512GFNI, archsimd.X86.AVX512GFNI()},
		{CPUFeatureAVX512VAES, archsimd.X86.AVX512VAES()},
		{CPUFeatureAVX512VBMI, archsimd.X86.AVX512VBMI()},
		{CPUFeatureAVX512VBMI2, archsimd.X86.AVX512VBMI2()},
		{CPUFeatureAVX512VNNI, archsimd.X86.AVX512VNNI()},
		{CPUFeatureAVX512VPCLMULQDQ, archsimd.X86.AVX512VPCLMULQDQ()},
		{CPUFeatureAVX512VPOPCNTDQ, archsimd.X86.AVX512VPOPCNTDQ()},
		{CPUFeatureAVXAES, archsimd.X86.AVXAES()},
		{CPUFeatureAVXVNNI, archsimd.X86.AVXVNNI()},
		{CPUFeatureFMA, archsimd.X86.FMA()},
		{CPUFeatureSHA, archsimd.X86.SHA()},
		{CPUFeatureVAES, archsimd.X86.VAES()},
	}
	for _, probe := range probes {
		if probe.has {
			features |= probe.feature.mask()
		}
	}
	return features
}
