package main

import (
	"fmt"
	"math"
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
}

type Metrics struct {
	NsPerOp     float64
	MBPerSec    float64
	BytesPerOp  float64
	AllocsPerOp float64
}

type benchmarkContract struct {
	Group string
	Impl  string
}

var requiredOperations = []benchmarkContract{
	{Group: "valid", Impl: "simdjson"},
	{Group: "typed-reused", Impl: "simdjson-owned"},
	{Group: "dynamic-owned", Impl: "simdjson-owned"},
	{Group: "encode", Impl: "simdjson-owned"},
	{Group: "encode", Impl: "simdjson-compiled-reuse"},
	{Group: "dom", Impl: "simdjson"},
}

var compatibleRivals = []string{"go-json", "Segment", "jsoniter", "fastjson"}

func (p Publication) validate() error {
	if len(p.Metadata.Commit) != 40 || p.Metadata.Dirty {
		return fmt.Errorf("publication must identify one clean 40-character commit")
	}
	if p.Metadata.Samples <= 0 || p.Metadata.BenchTime == "" {
		return fmt.Errorf("missing sample contract")
	}
	if len(p.Results) == 0 || len(p.Crosslang) != 14 {
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
	crosslang := make(map[string]CrosslangResult, len(p.Crosslang))
	for _, result := range p.Crosslang {
		key := result.Implementation + "\x00" + result.Corpus
		if _, ok := crosslang[key]; ok {
			return fmt.Errorf("duplicate cross-language result for %s/%s", result.Implementation, result.Corpus)
		}
		if result.Digest == "" || result.NsPerOp <= 0 || math.IsNaN(result.NsPerOp) || math.IsInf(result.NsPerOp, 0) {
			return fmt.Errorf("invalid cross-language result for %s/%s", result.Implementation, result.Corpus)
		}
		crosslang[key] = result
	}
	for _, corpus := range corpusOrder {
		cpp, cppOK := crosslang["cpp\x00"+corpus]
		goResult, goOK := crosslang["go\x00"+corpus]
		if !cppOK || !goOK || cpp.Digest != goResult.Digest {
			return fmt.Errorf("missing or mismatched cross-language result for %s", corpus)
		}
	}
	if err := p.validateSurfaces(); err != nil {
		return err
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
	require := func(variant, name string) error {
		if !p.hasBenchmark(variant, name) {
			return fmt.Errorf("missing benchmark %s/%s", variant, name)
		}
		return nil
	}
	for _, corpus := range corpusOrder {
		for _, spec := range requiredOperations {
			for _, required := range []struct{ variant, impl string }{
				{variant: "simd", impl: "encoding-json"},
				{variant: "simd", impl: spec.Impl},
				{variant: "pure", impl: spec.Impl},
			} {
				if err := require(required.variant, benchmarkName("BenchmarkStdlibCorpus", corpus, spec.Group, required.impl)); err != nil {
					return err
				}
			}
			if spec.Group != "dom" {
				if !p.hasCompatibleRival(corpus, spec.Group) {
					return fmt.Errorf("missing compatible rival for %s/%s", corpus, spec.Group)
				}
			}
		}
		for _, spec := range []struct{ variant, group, impl string }{
			{variant: "simd", group: "dynamic-owned", impl: "simdjson-zero-copy"},
			{variant: "pure", group: "dynamic-owned", impl: "simdjson-zero-copy"},
			{variant: "simd", group: "typed-reused", impl: "simdjson-zero-copy"},
			{variant: "pure", group: "typed-reused", impl: "simdjson-zero-copy"},
		} {
			if err := require(spec.variant, benchmarkName("BenchmarkStdlibCorpus", corpus, spec.group, spec.impl)); err != nil {
				return err
			}
		}
		indexName := benchmarkName("BenchmarkStdlibCorpusNativeParse", corpus, "", "simdjson-index-reused")
		if err := require("index-simd", indexName); err != nil {
			return err
		}
		if err := require("index-pure", indexName); err != nil {
			return err
		}
		for _, group := range []string{"valid", "dynamic-owned", "typed-reused", "encode"} {
			if err := require("sonic", benchmarkName("BenchmarkStdlibCorpusNativeSonic", corpus, group, sonicImplementation(group))); err != nil {
				return err
			}
		}
		for _, group := range []string{"dynamic-owned", "typed-reused", "encode"} {
			if err := require("jsonv2", benchmarkName("BenchmarkStdlibCorpusJSONV2", corpus, group, "jsonv2")); err != nil {
				return err
			}
		}
	}
	for _, benchmark := range []string{"BenchmarkHookDecodeSmall", "BenchmarkHookDecodeLarge", "BenchmarkHookEncodeSmall", "BenchmarkHookEncodeLarge"} {
		for _, implementation := range []string{"interpreter", "hook"} {
			if err := require("hooks", benchmark+"/"+implementation); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p Publication) hasBenchmark(variant, name string) bool {
	for _, result := range p.Results {
		if result.Variant == variant && result.Name == name {
			return true
		}
	}
	return false
}

func (p Publication) hasCompatibleRival(corpus, group string) bool {
	prefix := benchmarkName("BenchmarkStdlibCorpus", corpus, group, "") + "/"
	for _, result := range p.Results {
		if result.Variant != "simd" || !strings.HasPrefix(result.Name, prefix) {
			continue
		}
		implementation := strings.TrimPrefix(result.Name, prefix)
		for _, rival := range compatibleRivals {
			if implementation == rival {
				return true
			}
		}
	}
	return false
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

var corpusOrder = []string{
	"canada_geometry",
	"citm_catalog",
	"golang_source",
	"string_escaped",
	"string_unicode",
	"synthea_fhir",
	"twitter_status",
}
