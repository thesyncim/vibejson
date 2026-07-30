package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestParseAndAggregate(t *testing.T) {
	dir := t.TempDir()
	portable := filepath.Join(dir, "portable.txt")
	var lines strings.Builder
	for _, operation := range operations {
		for _, corpus := range corpusOrder {
			for _, series := range seriesForOperation(operation.id) {
				if series.mode != "portable" {
					continue
				}
				for sample := 1; sample <= 2; sample++ {
					lines.WriteString("BenchmarkComparisonCorpus/" + corpus + "/" + operation.id + "/" + series.library +
						"-1 1 " + strconv.Itoa(sample) + " ns/op 2 B/op 3 allocs/op\n")
				}
			}
		}
	}
	if err := os.WriteFile(portable, []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	simd := filepath.Join(dir, "simd.txt")
	lines.Reset()
	for _, operation := range operations {
		for _, corpus := range corpusOrder {
			for sample := 1; sample <= 2; sample++ {
				lines.WriteString("BenchmarkComparisonCorpus/" + corpus + "/" + operation.id +
					"/vibejson-1 1 " + strconv.Itoa(sample) + " ns/op 2 B/op 3 allocs/op\n")
			}
		}
	}
	if err := os.WriteFile(simd, []byte(lines.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	samples := make(map[metricKey][]sample)
	if err := parseBenchmarkFile(portable, "portable", samples); err != nil {
		t.Fatal(err)
	}
	if err := parseBenchmarkFile(simd, "simd", samples); err != nil {
		t.Fatal(err)
	}
	publication, err := buildPublication(samples, Metadata{Samples: 2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	wantResults := 0
	for _, operation := range operations {
		wantResults += len(corpusOrder) * len(seriesForOperation(operation.id))
	}
	if got, want := len(publication.Results), wantResults; got != want {
		t.Fatalf("results = %d, want %d", got, want)
	}
	aggregate := findAggregate(publication, operations[0].id, seriesOrder[0].id)
	if aggregate.NsPerPass != 10.5 || aggregate.BytesPerPass != 14 || aggregate.AllocsPerPass != 21 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
	if chart := string(renderChart(publication, chartTime)); !strings.Contains(chart, "10 ns") {
		t.Fatalf("chart lacks absolute aggregate: %s", chart)
	}
	if chart := string(renderSIMDChart(publication)); !strings.Contains(chart, "Strict whole-document validation") ||
		!strings.Contains(chart, "faster than fastest strict peer") {
		t.Fatalf("SIMD chart lacks validation comparison: %s", chart)
	}
}

func TestParseAndRenderNumericPublication(t *testing.T) {
	dir := t.TempDir()
	portable := filepath.Join(dir, "numeric-portable.txt")
	simd := filepath.Join(dir, "numeric-simd.txt")

	writeNumericFixture := func(path, mode string) {
		t.Helper()
		var lines strings.Builder
		for _, workload := range numericWorkloads {
			for _, series := range numericSeriesOrder {
				if series.mode != mode {
					continue
				}
				for sample := 1; sample <= 2; sample++ {
					lines.WriteString("BenchmarkNumericDecodePublication/" + workload.id + "/" + series.library +
						"-1 1 " + strconv.Itoa(sample*100) +
						" ns/op 123 input-B/op 0 B/op 0 allocs/op 6 values/op\n")
				}
			}
		}
		if err := os.WriteFile(path, []byte(lines.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeNumericFixture(portable, "portable")
	writeNumericFixture(simd, "simd")

	samples := make(map[numericMetricKey][]numericSample)
	if err := parseNumericBenchmarkFile(portable, "portable", samples); err != nil {
		t.Fatal(err)
	}
	if err := parseNumericBenchmarkFile(simd, "simd", samples); err != nil {
		t.Fatal(err)
	}
	publication, err := buildNumericPublication(samples, NumericMetadata{Samples: 2}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(publication.Results), len(numericWorkloads)*len(numericSeriesOrder); got != want {
		t.Fatalf("results = %d, want %d", got, want)
	}
	result := findNumericResult(publication, numericWorkloads[0].id, numericSeriesOrder[0])
	if result.NsPerOp != 150 || result.InputBytes != 123 || result.Values != 6 ||
		result.BytesPerOp != 0 || result.AllocsPerOp != 0 {
		t.Fatalf("numeric result = %+v", result)
	}
	chart := string(renderNumericChart(publication))
	if !strings.Contains(chart, "numeric-heavy JSON arrays") ||
		!strings.Contains(chart, "Every bar starts at zero") {
		t.Fatalf("numeric chart lacks contract text: %s", chart)
	}

	key := numericMetricKey{
		mode: numericSeriesOrder[1].mode, workload: numericWorkloads[0].id,
		library: numericSeriesOrder[1].library,
	}
	original := slices.Clone(samples[key])
	for i := range samples[key] {
		samples[key][i].inputBytes++
	}
	if _, err := buildNumericPublication(samples, NumericMetadata{Samples: 2}, 2); err == nil {
		t.Fatal("numeric publication accepted mismatched input shape")
	}
	samples[key] = original
	for i := range samples[key] {
		samples[key][i].bytesPerOp = 1
	}
	if _, err := buildNumericPublication(samples, NumericMetadata{Samples: 2}, 2); err == nil {
		t.Fatal("numeric publication accepted an allocating row")
	}
}
