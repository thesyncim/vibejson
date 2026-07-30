package main

import (
	"os"
	"path/filepath"
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
