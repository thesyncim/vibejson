package scanner

// CPUFeature identifies a runtime CPU capability exposed by the compiler's
// experimental SIMD package.
type CPUFeature uint64

const (
	CPUFeatureNEON CPUFeature = 1 << iota
	CPUFeaturePMULL
	CPUFeatureAVX
	CPUFeatureAVX2
	CPUFeatureAVX512
	CPUFeatureAVX512BITALG
	CPUFeatureAVX512GFNI
	CPUFeatureAVX512VAES
	CPUFeatureAVX512VBMI
	CPUFeatureAVX512VBMI2
	CPUFeatureAVX512VNNI
	CPUFeatureAVX512VPCLMULQDQ
	CPUFeatureAVX512VPOPCNTDQ
	CPUFeatureAVXAES
	CPUFeatureAVXVNNI
	CPUFeatureFMA
	CPUFeatureSHA
	CPUFeatureVAES
)

// CPUFeatures is an allocation-free bit set of detected CPU features.
type CPUFeatures uint64

var cpuFeatureNames = [...]struct {
	feature CPUFeature
	name    string
}{
	{CPUFeatureNEON, "neon"},
	{CPUFeaturePMULL, "pmull"},
	{CPUFeatureAVX, "avx"},
	{CPUFeatureAVX2, "avx2"},
	{CPUFeatureAVX512, "avx512"},
	{CPUFeatureAVX512BITALG, "avx512-bitalg"},
	{CPUFeatureAVX512GFNI, "avx512-gfni"},
	{CPUFeatureAVX512VAES, "avx512-vaes"},
	{CPUFeatureAVX512VBMI, "avx512-vbmi"},
	{CPUFeatureAVX512VBMI2, "avx512-vbmi2"},
	{CPUFeatureAVX512VNNI, "avx512-vnni"},
	{CPUFeatureAVX512VPCLMULQDQ, "avx512-vpclmulqdq"},
	{CPUFeatureAVX512VPOPCNTDQ, "avx512-vpopcntdq"},
	{CPUFeatureAVXAES, "avx-aes"},
	{CPUFeatureAVXVNNI, "avx-vnni"},
	{CPUFeatureFMA, "fma"},
	{CPUFeatureSHA, "sha"},
	{CPUFeatureVAES, "vaes"},
}

func (f CPUFeature) mask() CPUFeatures {
	return CPUFeatures(f)
}

// Has reports whether feature is present in f.
func (f CPUFeatures) Has(feature CPUFeature) bool {
	return f&feature.mask() != 0
}

// AppendNames appends stable feature names to dst.
func (f CPUFeatures) AppendNames(dst []string) []string {
	for _, item := range cpuFeatureNames {
		if f.Has(item.feature) {
			dst = append(dst, item.name)
		}
	}
	return dst
}

// Info describes the scanner implementation selected during initialization.
type Info struct {
	Enabled     bool
	Backend     string
	VectorBytes int
	MinBytes    int
	CPUFeatures CPUFeatures
}

// Current reports the selected scanner backend and detected CPU features.
func Current() Info {
	return currentInfo()
}
