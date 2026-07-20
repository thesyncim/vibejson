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
	want := benchmarkName("BenchmarkStdlibCorpus", corpusOrder[0], benchmarkContracts[0].Group, benchmarkContracts[0].SIMDImplementation)
	for i, result := range p.Results {
		if result.Variant == "simd" && result.Name == want {
			p.Results = append(p.Results[:i], p.Results[i+1:]...)
			break
		}
	}
	if err := p.validate(); err == nil || !strings.Contains(err.Error(), "benchmark set mismatch") {
		t.Fatalf("missing required contract error = %v", err)
	}
}

func TestPublicationRejectsModeOnlyBenchmark(t *testing.T) {
	p := testPublication()
	p.Results = append(p.Results, BenchmarkResult{
		Variant: "pure", Name: "BenchmarkStdlibCorpus/extra/valid/extra",
		NsPerOp: []float64{1, 1}, MBPerSec: []float64{0, 0},
		BytesPerOp: []float64{0, 0}, AllocsPerOp: []float64{0, 0},
	})
	if err := p.validate(); err == nil || !strings.Contains(err.Error(), "benchmark set mismatch") {
		t.Fatalf("mode-only benchmark error = %v", err)
	}
}

func TestPublicationRejectsOneSidedIndexPeer(t *testing.T) {
	p := testPublication()
	for _, corpus := range corpusOrder {
		name := benchmarkName("BenchmarkStdlibCorpusNativeParse", corpus, "", "minio-simdjson-go-reused-zero-copy")
		p.Results = append(p.Results, BenchmarkResult{
			Variant: "index-pure", Name: name,
			NsPerOp: []float64{1, 1}, MBPerSec: []float64{0, 0},
			BytesPerOp: []float64{0, 0}, AllocsPerOp: []float64{0, 0},
		})
	}
	if err := p.validate(); err == nil || !strings.Contains(err.Error(), "unmatched index") {
		t.Fatalf("one-sided index peer error = %v", err)
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
	for i := range p.Crosslang {
		if p.Crosslang[i].Implementation == "go-pure" {
			p.Crosslang[i].Digest = "fedcba9876543210"
			break
		}
	}
	if err := p.validate(); err == nil || !strings.Contains(err.Error(), "mismatched cross-language") {
		t.Fatalf("digest mismatch error = %v", err)
	}
}

func TestPublicationRejectsMalformedProvenance(t *testing.T) {
	for name, mutate := range map[string]func(*Publication){
		"short Go commit": func(p *Publication) { p.Metadata.GoCommit = "abcd" },
		"short digest":    func(p *Publication) { p.Crosslang[0].Digest = "abcd" },
		"missing machine": func(p *Publication) { p.Metadata.Machine = "" },
	} {
		t.Run(name, func(t *testing.T) {
			p := testPublication()
			mutate(&p)
			if err := p.validate(); err == nil {
				t.Fatal("malformed provenance was accepted")
			}
		})
	}
}

func TestPublicationRejectsBackendMismatch(t *testing.T) {
	for name, mutate := range map[string]func(*Publication){
		"SIMD scalar": func(p *Publication) {
			for i := range p.Crosslang {
				if p.Crosslang[i].Implementation == "go-simd" {
					p.Crosslang[i].Backend = "structural:scalar,arch:arm64/v8.0"
				}
			}
		},
		"mixed pure": func(p *Publication) {
			for i := range p.Crosslang {
				if p.Crosslang[i].Implementation == "go-pure" {
					p.Crosslang[i].Backend = "structural:scalar,arch:arm64/v8.1"
					break
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := testPublication()
			mutate(&p)
			if err := p.validate(); err == nil || !strings.Contains(err.Error(), "backend") {
				t.Fatalf("backend mismatch error = %v", err)
			}
		})
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
		CrossSamples:  6,
		CrossMinTime:  "250ms",
		CXXVersion:    "clang version 21",
		CXXLibrary:    "simdjson 4.6.4",
		CXXCommit:     strings.Repeat("c", 40),
		CXXImpl:       "arm64",
		GoExperiment:  "nosimd,simd",
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
		for _, spec := range benchmarkContracts {
			for _, variant := range []string{"simd", "pure"} {
				for _, impl := range spec.Implementations {
					name := benchmarkName("BenchmarkStdlibCorpus", corpus, spec.Group, impl)
					key := variant + "\x00" + name
					if seen[key] {
						continue
					}
					seen[key] = true
					ns := 100.0
					if impl == "encoding-json" {
						ns = 300
					} else if impl == "go-json" {
						ns = 200
					} else if variant == "pure" && strings.HasPrefix(impl, "simdjson") {
						ns = 150
					}
					add(variant, name, ns)
				}
			}
		}
		indexName := benchmarkName("BenchmarkStdlibCorpusNativeParse", corpus, "", "simdjson-index-reused")
		add("index-simd", indexName, 80)
		add("index-pure", indexName, 120)
		for _, group := range []string{"valid", "dynamic-owned", "typed-reused", "encode"} {
			for _, implementation := range sonicImplementations(group) {
				ns := 180.0
				if implementation == "encoding-json" {
					ns = 300
				}
				add("sonic", benchmarkName("BenchmarkStdlibCorpusNativeSonic", corpus, group, implementation), ns)
			}
		}
		for _, variant := range []string{"jsonv2-pure", "jsonv2-simd"} {
			for _, group := range []string{"dynamic-owned", "typed-reused", "encode"} {
				add(variant, benchmarkName("BenchmarkStdlibCorpusJSONV2", corpus, group, "encoding-json"), 300)
				add(variant, benchmarkName("BenchmarkStdlibCorpusJSONV2", corpus, group, "jsonv2"), 250)
			}
		}
		for _, impl := range []string{"cpp", "go-pure", "go-simd"} {
			backend := ""
			if impl == "go-pure" {
				backend = "structural:scalar,arch:arm64/v8.0"
			} else if impl == "go-simd" {
				backend = "structural:arm64-neon,arch:arm64/v8.0"
			}
			p.Crosslang = append(p.Crosslang, CrosslangResult{Implementation: impl, Corpus: corpus, Digest: "0123456789abcdef", NsPerOp: map[string]float64{"cpp": 120, "go-pure": 110, "go-simd": 100}[impl], Backend: backend})
		}
	}
	for _, benchmark := range []string{"BenchmarkHookDecodeSmall", "BenchmarkHookDecodeLarge", "BenchmarkHookEncodeSmall", "BenchmarkHookEncodeLarge"} {
		add("hooks", benchmark+"/interpreter", 100)
		add("hooks", benchmark+"/hook", 125)
	}
	return p
}
