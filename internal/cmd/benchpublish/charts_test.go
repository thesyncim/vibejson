package main

import (
	"bytes"
	"encoding/xml"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMedianUsesMiddlePairWithoutMutation(t *testing.T) {
	values := []float64{9, 1, 7, 3}
	original := slices.Clone(values)
	if got := median(values); got != 5 {
		t.Fatalf("median = %v, want 5", got)
	}
	if !slices.Equal(values, original) {
		t.Fatalf("median mutated input: %v", values)
	}
}

func TestSIMDChartUsesPerCorpusGeomean(t *testing.T) {
	publication := testPublication()
	rows, err := buildSIMDChartRows(publication)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(simdChartSpecs) {
		t.Fatalf("rows = %d, want %d", len(rows), len(simdChartSpecs))
	}
	if rows[0].wins != len(corpusOrder) || math.Abs(rows[0].ratio-1.5) > 1e-12 {
		t.Fatalf("validation row = %+v, want 7 wins and 1.5x", rows[0])
	}
}

func TestSIMDChartCountsLosingCorpus(t *testing.T) {
	publication := testPublication()
	name := benchmarkName("BenchmarkStdlibCorpus", corpusOrder[0], "valid", "simdjson")
	for i := range publication.Results {
		if publication.Results[i].Variant == "simd" && publication.Results[i].Name == name {
			publication.Results[i].NsPerOp = []float64{200, 200}
			break
		}
	}
	rows, err := buildSIMDChartRows(publication)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].wins != len(corpusOrder)-1 {
		t.Fatalf("validation wins = %d, want %d", rows[0].wins, len(corpusOrder)-1)
	}
}

func TestRenderChartsAccessibleAndDeterministic(t *testing.T) {
	publication := testPublication()
	first, err := renderCharts("/repo", publication)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderCharts("/repo", publication)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{
		filepath.Join("/repo", "benchmarks", "charts", "go-contracts.svg"),
		filepath.Join("/repo", "benchmarks", "charts", "simd-uplift.svg"),
		filepath.Join("/repo", "benchmarks", "crosslang", "chart.svg"),
	}
	if len(first) != len(wantPaths) {
		t.Fatalf("charts = %d, want %d", len(first), len(wantPaths))
	}
	if len(first) != len(second) {
		t.Fatalf("deterministic chart counts differ: %d and %d", len(first), len(second))
	}
	for i, path := range wantPaths {
		if first[i].path != path || second[i].path != path {
			t.Fatalf("chart %d path = %q/%q, want %q", i, first[i].path, second[i].path, path)
		}
		data := first[i].data
		if !bytes.Equal(data, second[i].data) {
			t.Fatalf("%s is nondeterministic", path)
		}
		if !bytes.HasSuffix(data, []byte{'\n'}) || !bytes.Contains(data, []byte(`<title id="title">`)) ||
			!bytes.Contains(data, []byte(`<desc id="desc">`)) || !bytes.Contains(data, []byte(`role="img"`)) {
			t.Fatalf("%s lacks accessible SVG metadata", path)
		}
		if bytes.Contains(data, []byte("NaN")) || bytes.Contains(data, []byte("Inf")) {
			t.Fatalf("%s contains a non-finite value", path)
		}
		if !bytes.Contains(data, []byte("benchmarks/results/latest.json")) {
			t.Fatalf("%s description does not point to raw results", path)
		}
		var document struct{ XMLName xml.Name }
		if err := xml.Unmarshal(data, &document); err != nil {
			t.Fatalf("%s is not XML: %v", path, err)
		}
	}
	goChart := string(first[0].data)
	for _, label := range []string{"simdjson", "encoding/json", "go-json", "Segment", "jsoniter", "fastjson", "encoding/json/v2", "Sonic (Go 1.26)"} {
		if !strings.Contains(goChart, label) {
			t.Errorf("Go chart omits %q", label)
		}
	}
}

func TestRatioTicksPreserveQuarterSteps(t *testing.T) {
	for value, want := range map[float64]string{0: "0x", 0.25: "0.25x", 0.5: "0.5x", 1: "1x", 1.25: "1.25x"} {
		if got := formatRatioTick(value); got != want {
			t.Errorf("formatRatioTick(%v) = %q, want %q", value, got, want)
		}
	}
}

func TestCrosslangChartRejectsMismatchedDigest(t *testing.T) {
	publication := testPublication()
	for i := range publication.Crosslang {
		if publication.Crosslang[i].Implementation == "go-pure" {
			publication.Crosslang[i].Digest = "different"
			break
		}
	}
	if err := validateCrosslangChart(publication); err == nil || !strings.Contains(err.Error(), "mismatched digest") {
		t.Fatalf("digest mismatch error = %v", err)
	}
}
