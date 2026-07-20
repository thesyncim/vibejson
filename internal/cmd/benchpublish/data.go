package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
)

type Publication struct {
	Metadata  Metadata          `json:"metadata"`
	Results   []BenchmarkResult `json:"results"`
	Crosslang []CrosslangResult `json:"cross_language"`
}

type Metadata struct {
	Commit        string `json:"commit"`
	Dirty         bool   `json:"dirty"`
	GoVersion     string `json:"go_version"`
	GoCommit      string `json:"go_commit"`
	LegacyVersion string `json:"legacy_go_version"`
	Machine       string `json:"machine"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Samples       int    `json:"samples"`
	BenchTime     string `json:"bench_time"`
	CrossSamples  int    `json:"cross_language_samples"`
	CrossMinTime  string `json:"cross_language_min_time"`
	CXXVersion    string `json:"cxx_version"`
	CXXLibrary    string `json:"cxx_library"`
	CXXCommit     string `json:"cxx_commit"`
	CXXImpl       string `json:"cxx_implementation"`
	GoExperiment  string `json:"go_experiment"`
}

type BenchmarkResult struct {
	Variant     string    `json:"variant"`
	Name        string    `json:"name"`
	NsPerOp     []float64 `json:"ns_per_op"`
	MBPerSec    []float64 `json:"mb_per_sec,omitempty"`
	BytesPerOp  []float64 `json:"bytes_per_op,omitempty"`
	AllocsPerOp []float64 `json:"allocs_per_op,omitempty"`
}

type CrosslangResult struct {
	Implementation string  `json:"implementation"`
	Corpus         string  `json:"corpus"`
	Digest         string  `json:"digest"`
	NsPerOp        float64 `json:"ns_per_op"`
	Backend        string  `json:"backend,omitempty"`
}

type Metrics struct {
	NsPerOp     float64
	MBPerSec    float64
	BytesPerOp  float64
	AllocsPerOp float64
}

// benchmarkContract is the single source of truth for the matched main-module
// benchmark matrix. ChartLabel selects the smaller README comparison view.
type benchmarkContract struct {
	Group              string
	ChartLabel         string
	SIMDImplementation string
	Implementations    []string
}

var benchmarkContracts = []benchmarkContract{
	{Group: "valid", ChartLabel: "Accepted-input scan", SIMDImplementation: "simdjson", Implementations: []string{"encoding-json", "Segment", "fastjson", "go-json", "jsoniter", "simdjson"}},
	{Group: "typed-reused", ChartLabel: "Typed owned decode (reused dst)", SIMDImplementation: "simdjson-owned", Implementations: []string{"encoding-json", "Segment", "go-json", "jsoniter", "simdjson-owned", "simdjson-zero-copy"}},
	{Group: "dynamic-owned", ChartLabel: "Dynamic owned decode", SIMDImplementation: "simdjson-owned", Implementations: []string{"encoding-json", "Segment", "go-json", "jsoniter", "simdjson-owned", "simdjson-zero-copy"}},
	{Group: "encode", ChartLabel: "Owned encode", SIMDImplementation: "simdjson-owned", Implementations: []string{"encoding-json", "Segment", "go-json", "jsoniter", "simdjson-compiled-reuse", "simdjson-owned"}},
	{Group: "dom", SIMDImplementation: "simdjson", Implementations: []string{"encoding-json", "simdjson"}},
}

func (p Publication) validate() error {
	if err := validateHex("repository commit", p.Metadata.Commit, 40); err != nil {
		return err
	}
	if p.Metadata.Dirty {
		return fmt.Errorf("publication must identify a clean repository commit")
	}
	if err := validateHex("Go commit", p.Metadata.GoCommit, 40); err != nil {
		return err
	}
	if err := validateHex("C++ commit", p.Metadata.CXXCommit, 40); err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{name: "Go version", value: p.Metadata.GoVersion},
		{name: "legacy Go version", value: p.Metadata.LegacyVersion},
		{name: "machine", value: p.Metadata.Machine},
		{name: "OS", value: p.Metadata.OS},
		{name: "architecture", value: p.Metadata.Arch},
		{name: "C++ version", value: p.Metadata.CXXVersion},
		{name: "C++ library", value: p.Metadata.CXXLibrary},
		{name: "C++ implementation", value: p.Metadata.CXXImpl},
		{name: "Go experiment", value: p.Metadata.GoExperiment},
	} {
		if field.value == "" {
			return fmt.Errorf("missing %s provenance", field.name)
		}
	}
	if p.Metadata.Samples <= 0 || p.Metadata.BenchTime == "" {
		return fmt.Errorf("missing Go benchmark sample contract")
	}
	if p.Metadata.CrossSamples <= 0 || p.Metadata.CrossMinTime == "" {
		return fmt.Errorf("missing cross-language sample contract")
	}
	if len(p.Results) == 0 || len(p.Crosslang) != 21 {
		return fmt.Errorf("incomplete result sets: Go=%d cross-language=%d", len(p.Results), len(p.Crosslang))
	}
	seen := make(map[string]bool, len(p.Results))
	for _, result := range p.Results {
		key := result.Variant + "\x00" + result.Name
		if seen[key] {
			return fmt.Errorf("duplicate benchmark %s/%s", result.Variant, result.Name)
		}
		seen[key] = true
		if err := validateSamples(result, p.Metadata.Samples); err != nil {
			return err
		}
	}
	if err := p.validateCrosslang(); err != nil {
		return err
	}
	return p.validateSurfaces()
}

func (p Publication) validateCrosslang() error {
	crosslang := make(map[string]CrosslangResult, len(p.Crosslang))
	backendByImplementation := make(map[string]string, 2)
	for _, result := range p.Crosslang {
		key := result.Implementation + "\x00" + result.Corpus
		if _, ok := crosslang[key]; ok {
			return fmt.Errorf("duplicate cross-language result for %s/%s", result.Implementation, result.Corpus)
		}
		if err := validateHex("cross-language digest", result.Digest, 16); err != nil {
			return fmt.Errorf("%w for %s/%s", err, result.Implementation, result.Corpus)
		}
		if result.NsPerOp <= 0 || math.IsNaN(result.NsPerOp) || math.IsInf(result.NsPerOp, 0) {
			return fmt.Errorf("invalid cross-language result for %s/%s", result.Implementation, result.Corpus)
		}
		if strings.HasPrefix(result.Implementation, "go-") {
			if result.Backend == "" {
				return fmt.Errorf("missing Go backend for cross-language result %s/%s", result.Implementation, result.Corpus)
			}
			if prior, ok := backendByImplementation[result.Implementation]; ok && prior != result.Backend {
				return fmt.Errorf("inconsistent Go backend for %s: %q and %q", result.Implementation, prior, result.Backend)
			}
			backendByImplementation[result.Implementation] = result.Backend
		} else if result.Backend != "" {
			return fmt.Errorf("unexpected backend for cross-language result %s/%s", result.Implementation, result.Corpus)
		}
		crosslang[key] = result
	}
	for _, implementation := range []string{"go-pure", "go-simd"} {
		if err := validateGoBackend(implementation, backendByImplementation[implementation], p.Metadata.Arch); err != nil {
			return err
		}
	}
	for _, corpus := range corpusOrder {
		cpp, cppOK := crosslang["cpp\x00"+corpus]
		goPure, pureOK := crosslang["go-pure\x00"+corpus]
		goSIMD, simdOK := crosslang["go-simd\x00"+corpus]
		if !cppOK || !pureOK || !simdOK || cpp.Digest != goPure.Digest || cpp.Digest != goSIMD.Digest {
			return fmt.Errorf("missing or mismatched cross-language result for %s", corpus)
		}
	}
	return nil
}

func validateHex(name, value string, size int) error {
	if len(value) != size {
		return fmt.Errorf("invalid %s: got %d characters, want %d", name, len(value), size)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("invalid %s: must be hexadecimal", name)
	}
	return nil
}

func validateGoBackend(implementation, backend, arch string) error {
	fields := make(map[string]string, 2)
	for _, field := range strings.Split(backend, ",") {
		name, value, ok := strings.Cut(field, ":")
		if !ok || name == "" || value == "" || fields[name] != "" {
			return fmt.Errorf("invalid Go backend for %s: %q", implementation, backend)
		}
		fields[name] = value
	}
	if len(fields) != 2 || fields["structural"] == "" || fields["arch"] == "" {
		return fmt.Errorf("invalid Go backend for %s: %q", implementation, backend)
	}
	if fields["arch"] != arch && !strings.HasPrefix(fields["arch"], arch+"/") {
		return fmt.Errorf("Go backend architecture mismatch for %s: %q, publication is %s", implementation, fields["arch"], arch)
	}
	want := "scalar"
	if implementation == "go-simd" {
		switch arch {
		case "arm64":
			want = "arm64-neon"
		case "amd64":
			if strings.Contains(fields["arch"], "/v3") || strings.Contains(fields["arch"], "/v4") {
				want = "amd64-avx2"
			}
		}
	}
	if fields["structural"] != want {
		return fmt.Errorf("invalid structural backend for %s: got %q, want %q", implementation, fields["structural"], want)
	}
	return nil
}

func validateSamples(result BenchmarkResult, count int) error {
	series := []struct {
		name     string
		values   []float64
		positive bool
	}{
		{name: "ns/op", values: result.NsPerOp, positive: true},
		{name: "MB/s", values: result.MBPerSec},
		{name: "B/op", values: result.BytesPerOp},
		{name: "allocs/op", values: result.AllocsPerOp},
	}
	for _, sample := range series {
		if len(sample.values) != count {
			return fmt.Errorf("invalid %s sample count for %s/%s: got %d, want %d", sample.name, result.Variant, result.Name, len(sample.values), count)
		}
		for _, value := range sample.values {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || (sample.positive && value == 0) {
				return fmt.Errorf("invalid %s sample for %s/%s: %v", sample.name, result.Variant, result.Name, value)
			}
		}
	}
	return nil
}

func (p Publication) validateSurfaces() error {
	allowedVariants := map[string]bool{
		"pure": true, "simd": true,
		"index-pure": true, "index-simd": true,
		"jsonv2-pure": true, "jsonv2-simd": true,
		"sonic": true, "hooks": true,
	}
	for _, result := range p.Results {
		if !allowedVariants[result.Variant] {
			return fmt.Errorf("unexpected benchmark variant %q", result.Variant)
		}
	}

	expectedMain := make([]string, 0, len(corpusOrder)*26)
	for _, corpus := range corpusOrder {
		for _, contract := range benchmarkContracts {
			for _, implementation := range contract.Implementations {
				expectedMain = append(expectedMain, benchmarkName("BenchmarkStdlibCorpus", corpus, contract.Group, implementation))
			}
		}
	}
	for _, variant := range []string{"pure", "simd"} {
		if err := p.validateExactVariant(variant, expectedMain); err != nil {
			return err
		}
	}

	baseIndex := expectedIndexBenchmarks(false)
	extendedIndex := expectedIndexBenchmarks(true)
	indexPure := p.variantNames("index-pure")
	indexSIMD := p.variantNames("index-simd")
	if !slices.Equal(indexPure, indexSIMD) {
		return fmt.Errorf("unmatched index benchmark sets between pure and SIMD modes")
	}
	if !slices.Equal(indexPure, baseIndex) && !slices.Equal(indexPure, extendedIndex) {
		return fmt.Errorf("unexpected index benchmark set")
	}

	sonic := make([]string, 0, len(corpusOrder)*9)
	jsonv2 := make([]string, 0, len(corpusOrder)*6)
	for _, corpus := range corpusOrder {
		for _, group := range []string{"valid", "dynamic-owned", "typed-reused", "encode"} {
			for _, implementation := range sonicImplementations(group) {
				sonic = append(sonic, benchmarkName("BenchmarkStdlibCorpusNativeSonic", corpus, group, implementation))
			}
		}
		for _, group := range []string{"dynamic-owned", "typed-reused", "encode"} {
			for _, implementation := range []string{"encoding-json", "jsonv2"} {
				jsonv2 = append(jsonv2, benchmarkName("BenchmarkStdlibCorpusJSONV2", corpus, group, implementation))
			}
		}
	}
	if err := p.validateExactVariant("sonic", sonic); err != nil {
		return err
	}
	for _, variant := range []string{"jsonv2-pure", "jsonv2-simd"} {
		if err := p.validateExactVariant(variant, jsonv2); err != nil {
			return err
		}
	}

	hooks := make([]string, 0, 8)
	for _, benchmark := range []string{"BenchmarkHookDecodeSmall", "BenchmarkHookDecodeLarge", "BenchmarkHookEncodeSmall", "BenchmarkHookEncodeLarge"} {
		for _, implementation := range []string{"interpreter", "hook"} {
			hooks = append(hooks, benchmark+"/"+implementation)
		}
	}
	return p.validateExactVariant("hooks", hooks)
}

func (p Publication) variantNames(variant string) []string {
	names := make([]string, 0)
	for _, result := range p.Results {
		if result.Variant == variant {
			names = append(names, result.Name)
		}
	}
	slices.Sort(names)
	return names
}

func (p Publication) validateExactVariant(variant string, expected []string) error {
	want := slices.Clone(expected)
	slices.Sort(want)
	got := p.variantNames(variant)
	if !slices.Equal(got, want) {
		return fmt.Errorf("benchmark set mismatch for %s: got %d rows, want %d", variant, len(got), len(want))
	}
	return nil
}

func expectedIndexBenchmarks(includeMinio bool) []string {
	implementations := []string{"simdjson-index-reused"}
	if includeMinio {
		implementations = append(implementations, "minio-simdjson-go-reused-zero-copy")
	}
	names := make([]string, 0, len(corpusOrder)*len(implementations))
	for _, corpus := range corpusOrder {
		for _, implementation := range implementations {
			names = append(names, benchmarkName("BenchmarkStdlibCorpusNativeParse", corpus, "", implementation))
		}
	}
	slices.Sort(names)
	return names
}

func benchmarkName(root, corpus, group, impl string) string {
	name := root + "/" + corpus
	if group != "" {
		name += "/" + group
	}
	if impl != "" {
		name += "/" + impl
	}
	return name
}

func sonicImplementation(group string) string {
	if group == "typed-reused" {
		return "Sonic-native-owned"
	}
	return "Sonic-native"
}

func sonicImplementations(group string) []string {
	if group == "typed-reused" {
		return []string{"encoding-json", "Sonic-native-owned", "Sonic-native-zero-copy"}
	}
	return []string{"encoding-json", "Sonic-native"}
}

var corpusOrder = []string{
	"canada_geometry",
	"citm_catalog",
	"golang_source",
	"string_escaped",
	"string_unicode",
	"synthea_fhir",
	"twitter_status",
}
