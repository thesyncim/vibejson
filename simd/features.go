package simd

import "github.com/thesyncim/simdjson/internal/scanner"

// CPUFeature identifies a CPU capability reported by Go's experimental
// archsimd package. Features are diagnostic, not routing promises: use the
// backend fields returned by Current to see which implementations execute.
// In particular, PMULL and AVX-512 are currently reporting-only capabilities.
type CPUFeature uint64

const (
	CPUFeatureNEON             CPUFeature = CPUFeature(scanner.CPUFeatureNEON)
	CPUFeaturePMULL            CPUFeature = CPUFeature(scanner.CPUFeaturePMULL)
	CPUFeatureAVX              CPUFeature = CPUFeature(scanner.CPUFeatureAVX)
	CPUFeatureAVX2             CPUFeature = CPUFeature(scanner.CPUFeatureAVX2)
	CPUFeatureAVX512           CPUFeature = CPUFeature(scanner.CPUFeatureAVX512)
	CPUFeatureAVX512BITALG     CPUFeature = CPUFeature(scanner.CPUFeatureAVX512BITALG)
	CPUFeatureAVX512GFNI       CPUFeature = CPUFeature(scanner.CPUFeatureAVX512GFNI)
	CPUFeatureAVX512VAES       CPUFeature = CPUFeature(scanner.CPUFeatureAVX512VAES)
	CPUFeatureAVX512VBMI       CPUFeature = CPUFeature(scanner.CPUFeatureAVX512VBMI)
	CPUFeatureAVX512VBMI2      CPUFeature = CPUFeature(scanner.CPUFeatureAVX512VBMI2)
	CPUFeatureAVX512VNNI       CPUFeature = CPUFeature(scanner.CPUFeatureAVX512VNNI)
	CPUFeatureAVX512VPCLMULQDQ CPUFeature = CPUFeature(scanner.CPUFeatureAVX512VPCLMULQDQ)
	CPUFeatureAVX512VPOPCNTDQ  CPUFeature = CPUFeature(scanner.CPUFeatureAVX512VPOPCNTDQ)
	CPUFeatureAVXAES           CPUFeature = CPUFeature(scanner.CPUFeatureAVXAES)
	CPUFeatureAVXVNNI          CPUFeature = CPUFeature(scanner.CPUFeatureAVXVNNI)
	CPUFeatureFMA              CPUFeature = CPUFeature(scanner.CPUFeatureFMA)
	CPUFeatureSHA              CPUFeature = CPUFeature(scanner.CPUFeatureSHA)
	CPUFeatureVAES             CPUFeature = CPUFeature(scanner.CPUFeatureVAES)
)

// CPUFeatures is an allocation-free bit set of capabilities reported by the
// active SIMD build. Portable builds report no features.
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

// AppendNames appends stable feature names to dst without allocating when dst
// has enough spare capacity.
func (f CPUFeatures) AppendNames(dst []string) []string {
	for _, item := range cpuFeatureNames {
		if f.Has(item.feature) {
			dst = append(dst, item.name)
		}
	}
	return dst
}

// Info describes the effective implementations and reported CPU capabilities
// for this build and process. Some implementations are fixed at compile time;
// an amd64 scanner in a GOAMD64 v1/v2 build may instead be chosen at package
// initialization.
type Info struct {
	Enabled           bool        // kernels compiled in and selected
	StringBackend     string      // string scanning implementation name
	FormatBackend     string      // digit formatting implementation name
	StringVectorBytes int         // string kernel vector width, 0 when scalar
	FormatVectorBytes int         // format kernel vector width, 0 when scalar
	StringMinBytes    int         // shortest input the string kernels accept
	Features          CPUFeatures // CPU capabilities reported for diagnostics
}

// Current reports the effective string and decimal-format implementations,
// together with diagnostic CPU capabilities. Backend names, rather than
// feature presence alone, identify the selected kernels.
func Current() Info {
	scan := scanner.Current()
	format := formatBackend()
	return Info{
		Enabled:           scan.Enabled || format != "scalar",
		StringBackend:     scan.Backend,
		FormatBackend:     format,
		StringVectorBytes: scan.VectorBytes,
		FormatVectorBytes: formatVectorBytes(),
		StringMinBytes:    scan.MinBytes,
		Features:          CPUFeatures(scan.CPUFeatures),
	}
}
