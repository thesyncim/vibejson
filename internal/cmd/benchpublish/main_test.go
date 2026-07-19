package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseBenchmarkLine(t *testing.T) {
	line := "BenchmarkStdlibCorpus/canada_geometry/valid/simdjson-1  123  456.5 ns/op  789.25 MB/s  16 B/op  2 allocs/op"
	name, metric, ok := parseBenchmarkLine(line)
	if !ok || name != "BenchmarkStdlibCorpus/canada_geometry/valid/simdjson" {
		t.Fatalf("parse name: %q, %v", name, ok)
	}
	if metric != (Metrics{NsPerOp: 456.5, MBPerSec: 789.25, BytesPerOp: 16, AllocsPerOp: 2}) {
		t.Fatalf("metrics = %+v", metric)
	}
}

func TestParseCrosslangLine(t *testing.T) {
	line := "canada_geometry size=  270000 contract=parse+semantic-digest digest=99bfa84117bedba4 time=    330125ns ( 0.82 GB/s)"
	result, err := parseCrosslangLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if result.Corpus != "canada_geometry" || result.Digest != "99bfa84117bedba4" || result.NsPerOp != 330125 {
		t.Fatalf("result = %+v", result)
	}
}

func TestPublicationValidatesRequiredContracts(t *testing.T) {
	p := testPublication()
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	want := benchmarkName("BenchmarkStdlibCorpus", corpusOrder[0], requiredOperations[0].Group, requiredOperations[0].Impl)
	for i, result := range p.Results {
		if result.Variant == "simd" && result.Name == want {
			p.Results = append(p.Results[:i], p.Results[i+1:]...)
			break
		}
	}
	if err := p.validate(); err == nil || !strings.Contains(err.Error(), "missing benchmark") {
		t.Fatalf("missing required contract error = %v", err)
	}
}

func TestPublicationRejectsDuplicateBenchmark(t *testing.T) {
	p := testPublication()
	p.Results = append(p.Results, p.Results[0])
	if err := p.validate(); err == nil || !strings.Contains(err.Error(), "duplicate benchmark") {
		t.Fatalf("duplicate benchmark error = %v", err)
	}
}

func TestPublicationRejectsInvalidSample(t *testing.T) {
	p := testPublication()
	p.Results[0].NsPerOp[0] = 0
	if err := p.validate(); err == nil || !strings.Contains(err.Error(), "invalid ns/op sample") {
		t.Fatalf("invalid sample error = %v", err)
	}
}

func TestPublicationRejectsMismatchedCrosslangDigest(t *testing.T) {
	p := testPublication()
	p.Crosslang[0].Digest = "different"
	if err := p.validate(); err == nil || !strings.Contains(err.Error(), "mismatched cross-language") {
		t.Fatalf("digest mismatch error = %v", err)
	}
}

func TestEncodePublicationIsStable(t *testing.T) {
	want, err := encodePublication(testPublication())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(want, []byte{'\n'}) {
		t.Fatal("normalized record has no final newline")
	}
	var decoded Publication
	if err := json.Unmarshal(want, &decoded); err != nil {
		t.Fatal(err)
	}
	got, err := encodePublication(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("normalized record changed after a round trip")
	}
}

func testPublication() Publication {
	p := Publication{Metadata: Metadata{
		Commit:        strings.Repeat("a", 40),
		GoVersion:     "go1.27-devel_test",
		GoCommit:      strings.Repeat("b", 40),
		LegacyVersion: "go1.26.4 darwin/arm64",
		Machine:       "Test CPU",
		OS:            "darwin",
		Arch:          "arm64",
		Samples:       2,
		BenchTime:     "300ms",
		CXXVersion:    "clang version 21",
		CXXLibrary:    "simdjson 4.6.4",
		CXXCommit:     strings.Repeat("c", 40),
		CXXImpl:       "arm64",
		GoExperiment:  "simd",
	}}
	add := func(variant, name string, ns float64) {
		p.Results = append(p.Results, BenchmarkResult{
			Variant: variant, Name: name,
			NsPerOp: []float64{ns, ns}, MBPerSec: []float64{2000, 2000},
			BytesPerOp: []float64{16, 16}, AllocsPerOp: []float64{1, 1},
		})
	}
	seen := make(map[string]bool)
	for _, corpus := range corpusOrder {
		for _, spec := range requiredOperations {
			for _, impl := range []string{"encoding-json", spec.Impl, "go-json"} {
				name := benchmarkName("BenchmarkStdlibCorpus", corpus, spec.Group, impl)
				key := "simd\x00" + name
				if seen[key] {
					continue
				}
				seen[key] = true
				ns := 100.0
				if impl == "encoding-json" {
					ns = 300
				} else if impl == "go-json" {
					ns = 200
				}
				add("simd", name, ns)
			}
			pureName := benchmarkName("BenchmarkStdlibCorpus", corpus, spec.Group, spec.Impl)
			pureKey := "pure\x00" + pureName
			if !seen[pureKey] {
				seen[pureKey] = true
				add("pure", pureName, 150)
			}
		}
		for _, spec := range []struct{ group, impl string }{
			{"dynamic-owned", "simdjson-zero-copy"},
			{"typed-reused", "simdjson-zero-copy"},
		} {
			name := benchmarkName("BenchmarkStdlibCorpus", corpus, spec.group, spec.impl)
			add("simd", name, 90)
			add("pure", name, 140)
		}
		indexName := benchmarkName("BenchmarkStdlibCorpusNativeParse", corpus, "", "simdjson-index-reused")
		add("index-simd", indexName, 80)
		add("index-pure", indexName, 120)
		for _, group := range []string{"valid", "dynamic-owned", "typed-reused", "encode"} {
			add("sonic", benchmarkName("BenchmarkStdlibCorpusNativeSonic", corpus, group, sonicImplementation(group)), 180)
		}
		for _, group := range []string{"dynamic-owned", "typed-reused", "encode"} {
			add("jsonv2", benchmarkName("BenchmarkStdlibCorpusJSONV2", corpus, group, "jsonv2"), 250)
		}
		for _, impl := range []string{"cpp", "go"} {
			p.Crosslang = append(p.Crosslang, CrosslangResult{Implementation: impl, Corpus: corpus, Digest: "0123456789abcdef", NsPerOp: map[string]float64{"cpp": 120, "go": 100}[impl]})
		}
	}
	for _, benchmark := range []string{"BenchmarkHookDecodeSmall", "BenchmarkHookDecodeLarge", "BenchmarkHookEncodeSmall", "BenchmarkHookEncodeLarge"} {
		add("hooks", benchmark+"/interpreter", 100)
		add("hooks", benchmark+"/hook", 125)
	}
	return p
}
