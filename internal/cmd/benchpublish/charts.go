package main

import (
	"fmt"
	"html"
	"math"
	"path/filepath"
	"slices"
	"strings"
)

const chartStyle = `<style>
svg { color-scheme: light dark; }
text { fill:#24292f; font:13px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
.heading { font-size:17px; font-weight:500; }
.note { font-size:12px; }
.muted { fill:#57606a; }
.grid { stroke:#d0d7de; stroke-width:1; }
.baseline { stroke:#8c959f; stroke-width:2; }
.portable { fill:#8250df; }
.simd { fill:#0969da; }
.reference { fill:#8c959f; }
.peer { fill:#1a7f37; }
.cell { fill:#f6f8fa; }
@media (prefers-color-scheme: dark) {
text { fill:#f0f6fc; }
.muted { fill:#8c959f; }
.grid { stroke:#30363d; }
.baseline { stroke:#6e7681; }
.portable { fill:#bc8cff; }
.simd { fill:#58a6ff; }
.reference { fill:#8c959f; }
.peer { fill:#3fb950; }
.cell { fill:#161b22; }
}
</style>`

type simdChartSpec struct {
	label       string
	root        string
	group       string
	impl        string
	pureVariant string
	simdVariant string
}

var simdChartSpecs = []simdChartSpec{
	{label: "Strict validation", root: "BenchmarkStdlibCorpus", group: "valid", impl: "simdjson", pureVariant: "pure", simdVariant: "simd"},
	{label: "Dynamic owned decode", root: "BenchmarkStdlibCorpus", group: "dynamic-owned", impl: "simdjson-owned", pureVariant: "pure", simdVariant: "simd"},
	{label: "Dynamic zero-copy", root: "BenchmarkStdlibCorpus", group: "dynamic-owned", impl: "simdjson-zero-copy", pureVariant: "pure", simdVariant: "simd"},
	{label: "Parse + complete walk", root: "BenchmarkStdlibCorpus", group: "dom", impl: "simdjson", pureVariant: "pure", simdVariant: "simd"},
	{label: "Typed owned decode (reused dst)", root: "BenchmarkStdlibCorpus", group: "typed-reused", impl: "simdjson-owned", pureVariant: "pure", simdVariant: "simd"},
	{label: "Typed zero-copy", root: "BenchmarkStdlibCorpus", group: "typed-reused", impl: "simdjson-zero-copy", pureVariant: "pure", simdVariant: "simd"},
	{label: "Owned encode", root: "BenchmarkStdlibCorpus", group: "encode", impl: "simdjson-owned", pureVariant: "pure", simdVariant: "simd"},
	{label: "Compiled encode reuse", root: "BenchmarkStdlibCorpus", group: "encode", impl: "simdjson-compiled-reuse", pureVariant: "pure", simdVariant: "simd"},
	{label: "Reusable structural index", root: "BenchmarkStdlibCorpusNativeParse", impl: "simdjson-index-reused", pureVariant: "index-pure", simdVariant: "index-simd"},
}

type simdChartRow struct {
	label string
	wins  int
	ratio float64
}

var goChartOperations = func() []benchmarkContract {
	operations := make([]benchmarkContract, 0, 4)
	for _, contract := range benchmarkContracts {
		if contract.ChartLabel != "" {
			operations = append(operations, contract)
		}
	}
	return operations
}()

type goChartSeries struct {
	label  string
	kind   string
	values [][2]float64
	valid  [][2]bool
}

func renderCharts(root string, publication Publication) ([]artifact, error) {
	simdRows, err := buildSIMDChartRows(publication)
	if err != nil {
		return nil, err
	}
	goRows, err := buildGoChartSeries(publication)
	if err != nil {
		return nil, err
	}
	if err := validateCrosslangChart(publication); err != nil {
		return nil, err
	}
	return []artifact{
		{path: filepath.Join(root, "benchmarks", "charts", "go-contracts.svg"), data: renderGoChart(publication, goRows)},
		{path: filepath.Join(root, "benchmarks", "charts", "simd-uplift.svg"), data: renderSIMDChart(publication, simdRows)},
		{path: filepath.Join(root, "benchmarks", "crosslang", "chart.svg"), data: renderCrosslangChart(publication)},
	}, nil
}

func buildSIMDChartRows(publication Publication) ([]simdChartRow, error) {
	rows := make([]simdChartRow, 0, len(simdChartSpecs))
	for _, spec := range simdChartSpecs {
		var ratios []float64
		wins := 0
		for _, corpus := range corpusOrder {
			name := benchmarkName(spec.root, corpus, spec.group, spec.impl)
			pure, ok := publication.metric(spec.pureVariant, name)
			if !ok {
				return nil, fmt.Errorf("SIMD chart: missing %s/%s", spec.pureVariant, name)
			}
			simd, ok := publication.metric(spec.simdVariant, name)
			if !ok {
				return nil, fmt.Errorf("SIMD chart: missing %s/%s", spec.simdVariant, name)
			}
			if simd < pure {
				wins++
			}
			ratios = append(ratios, pure/simd)
		}
		rows = append(rows, simdChartRow{label: spec.label, wins: wins, ratio: geomean(ratios)})
	}
	return rows, nil
}

func buildGoChartSeries(publication Publication) ([]goChartSeries, error) {
	type source struct {
		label string
		kind  string
		value func(Publication, string, benchmarkContract, string) (float64, bool)
	}
	paired := func(implementation string) func(Publication, string, benchmarkContract, string) (float64, bool) {
		return func(p Publication, mode string, operation benchmarkContract, corpus string) (float64, bool) {
			variant := mode
			name := benchmarkName("BenchmarkStdlibCorpus", corpus, operation.Group, implementation)
			value, ok := p.metric(variant, name)
			if !ok {
				return 0, false
			}
			baseline, ok := p.metric(variant, benchmarkName("BenchmarkStdlibCorpus", corpus, operation.Group, "encoding-json"))
			return baseline / value, ok
		}
	}
	simdjson := func(p Publication, mode string, operation benchmarkContract, corpus string) (float64, bool) {
		return paired(operation.SIMDImplementation)(p, mode, operation, corpus)
	}
	jsonv2 := func(p Publication, mode string, operation benchmarkContract, corpus string) (float64, bool) {
		if operation.Group == "valid" {
			return 0, false
		}
		variant := "jsonv2-" + mode
		value, ok := p.metric(variant, benchmarkName("BenchmarkStdlibCorpusJSONV2", corpus, operation.Group, "jsonv2"))
		if !ok {
			return 0, false
		}
		baseline, ok := p.metric(variant, benchmarkName("BenchmarkStdlibCorpusJSONV2", corpus, operation.Group, "encoding-json"))
		return baseline / value, ok
	}
	sonic := func(p Publication, _ string, operation benchmarkContract, corpus string) (float64, bool) {
		implementation := sonicImplementation(operation.Group)
		value, ok := p.metric("sonic", benchmarkName("BenchmarkStdlibCorpusNativeSonic", corpus, operation.Group, implementation))
		if !ok {
			return 0, false
		}
		baseline, ok := p.metric("sonic", benchmarkName("BenchmarkStdlibCorpusNativeSonic", corpus, operation.Group, "encoding-json"))
		return baseline / value, ok
	}
	sources := []source{
		{label: "simdjson", kind: "simdjson", value: simdjson},
		{label: "encoding/json", kind: "reference", value: paired("encoding-json")},
		{label: "go-json", kind: "peer", value: paired("go-json")},
		{label: "Segment", kind: "peer", value: paired("Segment")},
		{label: "jsoniter", kind: "peer", value: paired("jsoniter")},
		{label: "fastjson", kind: "peer", value: paired("fastjson")},
		{label: "encoding/json/v2", kind: "peer", value: jsonv2},
		{label: "Sonic (Go 1.26)", kind: "stable", value: sonic},
	}
	rows := make([]goChartSeries, 0, len(sources))
	for _, source := range sources {
		row := goChartSeries{label: source.label, kind: source.kind, values: make([][2]float64, len(goChartOperations)), valid: make([][2]bool, len(goChartOperations))}
		for operationIndex, operation := range goChartOperations {
			modes := []string{"pure", "simd"}
			if source.kind == "stable" {
				modes = []string{"stable"}
			}
			for modeIndex, mode := range modes {
				var ratios []float64
				complete := true
				for _, corpus := range corpusOrder {
					ratio, ok := source.value(publication, mode, operation, corpus)
					if !ok {
						complete = false
						break
					}
					ratios = append(ratios, ratio)
				}
				if complete {
					row.values[operationIndex][modeIndex] = geomean(ratios)
					row.valid[operationIndex][modeIndex] = true
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func validateCrosslangChart(publication Publication) error {
	for _, corpus := range corpusOrder {
		cpp, cppOK := publication.crosslangMetric("cpp", corpus)
		pure, pureOK := publication.crosslangMetric("go-pure", corpus)
		simd, simdOK := publication.crosslangMetric("go-simd", corpus)
		if !cppOK || !pureOK || !simdOK {
			return fmt.Errorf("cross-language chart: incomplete implementations for %s", corpus)
		}
		if cpp.Digest != pure.Digest || cpp.Digest != simd.Digest {
			return fmt.Errorf("cross-language chart: mismatched digest for %s", corpus)
		}
	}
	return nil
}

func renderSIMDChart(publication Publication, rows []simdChartRow) []byte {
	const width = 1000
	height := 118 + len(rows)*36
	left, right, top := 270.0, 190.0, 84.0
	maxRatio := 1.25
	for _, row := range rows {
		maxRatio = math.Max(maxRatio, row.ratio)
	}
	maxRatio = math.Ceil(maxRatio*4) / 4
	plotWidth := float64(width) - left - right
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title desc">`, width, height, width, height)
	out.WriteString(`<title id="title">SIMD uplift over portable Go</title>`)
	fmt.Fprintf(&out, `<desc id="desc">Geometric-mean portable-Go time divided by SIMD time over seven corpus files. Values above one mean SIMD is faster. %s Source: benchmarks/results/latest.json. Snapshot %s.</desc>`, html.EscapeString(simdChartSummary(rows)), html.EscapeString(chartProvenance(publication)))
	out.WriteString(chartStyle)
	out.WriteString(`<text class="heading" x="8" y="22">SIMD uplift over portable Go</text>`)
	fmt.Fprintf(&out, `<text class="muted note" x="8" y="43">Geomean across seven payloads · 1x is equal · higher is faster · %s</text>`, html.EscapeString(chartProvenance(publication)))
	for tick := 0.0; tick <= maxRatio+0.001; tick += 0.25 {
		x := left + tick/maxRatio*plotWidth
		class := "grid"
		if math.Abs(tick-1) < 0.001 {
			class = "baseline"
		}
		fmt.Fprintf(&out, `<line class="%s" x1="%.1f" y1="62" x2="%.1f" y2="%d"/><text class="muted note" x="%.1f" y="78" text-anchor="middle">%s</text>`, class, x, x, height-22, x, formatRatioTick(tick))
	}
	for i, row := range rows {
		y := top + float64(i)*36
		barWidth := row.ratio / maxRatio * plotWidth
		fmt.Fprintf(&out, `<text x="8" y="%.1f">%s</text><rect class="simd" x="%.1f" y="%.1f" width="%.1f" height="18" rx="2"/><text x="%.1f" y="%.1f">%.3fx</text><text class="muted note" x="%d" y="%.1f" text-anchor="end">%d/7 wins</text>`, y+14, html.EscapeString(row.label), left, y, barWidth, left+barWidth+8, y+14, row.ratio, width-8, y+14, row.wins)
	}
	out.WriteString(`</svg>`)
	return append([]byte(out.String()), '\n')
}

func renderGoChart(publication Publication, rows []goChartSeries) []byte {
	const width = 1100
	const labelWidth = 180
	const columnWidth = 225
	const rowHeight = 44
	height := 136 + len(rows)*rowHeight
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title desc">`, width, height, width, height)
	out.WriteString(`<title id="title">Go library comparison by measured operation</title>`)
	fmt.Fprintf(&out, `<desc id="desc">Geometric-mean speed relative to encoding/json across seven corpus files. Each regular cell shows portable then SIMD compiler modes. Higher is faster. %s Source: benchmarks/results/latest.json. Snapshot %s.</desc>`, html.EscapeString(goChartSummary(rows)), html.EscapeString(chartProvenance(publication)))
	out.WriteString(chartStyle)
	out.WriteString(`<text class="heading" x="8" y="22">Go library comparison by measured operation</text>`)
	fmt.Fprintf(&out, `<text class="muted note" x="8" y="43">Geomean speed vs matching encoding/json · portable / SIMD · higher is faster · %s</text>`, html.EscapeString(chartProvenance(publication)))
	out.WriteString(`<rect class="portable" x="8" y="55" width="12" height="12" rx="2"/><text class="note" x="26" y="66">portable</text><rect class="simd" x="100" y="55" width="12" height="12" rx="2"/><text class="note" x="118" y="66">SIMD</text><rect class="reference" x="176" y="55" width="12" height="12" rx="2"/><text class="note" x="194" y="66">stable-only context</text>`)
	for column, operation := range goChartOperations {
		x := labelWidth + column*columnWidth
		fmt.Fprintf(&out, `<text x="%d" y="96" text-anchor="middle">%s</text>`, x+columnWidth/2, html.EscapeString(operation.ChartLabel))
	}
	for rowIndex, row := range rows {
		y := 110 + rowIndex*rowHeight
		fmt.Fprintf(&out, `<text x="8" y="%d">%s</text>`, y+27, html.EscapeString(row.label))
		for column := range goChartOperations {
			x := labelWidth + column*columnWidth
			fmt.Fprintf(&out, `<rect class="cell" x="%d" y="%d" width="%d" height="36" rx="2"/>`, x+4, y+4, columnWidth-8)
			if row.kind == "stable" {
				if row.valid[column][0] {
					fmt.Fprintf(&out, `<rect class="reference" x="%d" y="%d" width="58" height="8" rx="2"/><text x="%d" y="%d">%.2fx stable</text>`, x+14, y+13, x+82, y+23, row.values[column][0])
				} else {
					fmt.Fprintf(&out, `<text class="muted" x="%d" y="%d" text-anchor="middle">—</text>`, x+columnWidth/2, y+27)
				}
				continue
			}
			if !row.valid[column][0] && !row.valid[column][1] {
				fmt.Fprintf(&out, `<text class="muted" x="%d" y="%d" text-anchor="middle">—</text>`, x+columnWidth/2, y+27)
				continue
			}
			fmt.Fprintf(&out, `<rect class="portable" x="%d" y="%d" width="54" height="8" rx="2"/><rect class="simd" x="%d" y="%d" width="54" height="8" rx="2"/><text x="%d" y="%d">%.2fx / %.2fx</text>`, x+12, y+10, x+12, y+24, x+76, y+27, row.values[column][0], row.values[column][1])
		}
	}
	out.WriteString(`<text class="muted note" x="8" y="`)
	fmt.Fprintf(&out, `%d">Accepted-input scan is throughput context, not rejection-equivalence proof. JSON/v2 has no scan row; Sonic uses its supported stable compiler.</text>`, height-12)
	out.WriteString(`</svg>`)
	return append([]byte(out.String()), '\n')
}

func renderCrosslangChart(publication Publication) []byte {
	const width, height = 1100, 390
	const plotTop, baseline, barWidth = 124, 300, 24
	facetWidth := float64(width) / float64(len(corpusOrder))
	var out strings.Builder
	firstCorpus := corpusOrder[0]
	pureBackend, _ := publication.crosslangMetric("go-pure", firstCorpus)
	simdBackend, _ := publication.crosslangMetric("go-simd", firstCorpus)
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title desc">`, width, height, width, height)
	out.WriteString(`<title id="title">C++ and Go parse plus semantic digest</title>`)
	fmt.Fprintf(&out, `<desc id="desc">Absolute completion time for matched semantic digests in C++ simdjson, portable Go, and SIMD Go. Every corpus has its own scale; lower is faster. %s Source: benchmarks/results/latest.json. Snapshot %s; C++ %s at %s.</desc>`, html.EscapeString(crosslangChartSummary(publication)), html.EscapeString(crosslangProvenance(publication)), html.EscapeString(publication.Metadata.CXXLibrary), html.EscapeString(publication.Metadata.CXXCommit))
	out.WriteString(chartStyle)
	out.WriteString(`<text class="heading" x="8" y="22">C++ and Go parse + semantic digest</text>`)
	fmt.Fprintf(&out, `<text class="muted note" x="8" y="43">Identical digest contract · absolute time · per-corpus scales · lower is faster · %s</text>`, html.EscapeString(crosslangProvenance(publication)))
	fmt.Fprintf(&out, `<rect class="reference" x="8" y="57" width="12" height="12" rx="2"/><text class="note" x="26" y="68">C++ simdjson</text><rect class="portable" x="140" y="57" width="12" height="12" rx="2"/><text class="note" x="158" y="68">Go portable (%s)</text><rect class="simd" x="438" y="57" width="12" height="12" rx="2"/><text class="note" x="456" y="68">Go SIMD (%s)</text>`, html.EscapeString(pureBackend.Backend), html.EscapeString(simdBackend.Backend))
	for index, corpus := range corpusOrder {
		cpp, _ := publication.crosslangMetric("cpp", corpus)
		pure, _ := publication.crosslangMetric("go-pure", corpus)
		simd, _ := publication.crosslangMetric("go-simd", corpus)
		values := []struct {
			class string
			value float64
		}{{class: "reference", value: cpp.NsPerOp}, {class: "portable", value: pure.NsPerOp}, {class: "simd", value: simd.NsPerOp}}
		facetMax := math.Max(cpp.NsPerOp, math.Max(pure.NsPerOp, simd.NsPerOp))
		center := (float64(index) + 0.5) * facetWidth
		fmt.Fprintf(&out, `<line class="grid" x1="%.1f" y1="%d" x2="%.1f" y2="%d"/>`, center-facetWidth/2+8, baseline, center+facetWidth/2-8, baseline)
		for series, item := range values {
			barHeight := item.value / facetMax * float64(baseline-plotTop)
			x := center - 44 + float64(series)*32
			y := float64(baseline) - barHeight
			labelY := y - 5 - float64(series)*14
			fmt.Fprintf(&out, `<rect class="%s" x="%.1f" y="%.1f" width="%d" height="%.1f" rx="2"/><text class="note" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, item.class, x, y, barWidth, barHeight, x+barWidth/2, labelY, formatCompactDuration(item.value))
		}
		fmt.Fprintf(&out, `<text x="%.1f" y="324" text-anchor="middle">%s</text><text class="muted note" x="%.1f" y="342" text-anchor="middle">%s</text>`, center, html.EscapeString(corpusLabel(corpus)), center, html.EscapeString(cpp.Digest[:8]))
	}
	out.WriteString(`</svg>`)
	return append([]byte(out.String()), '\n')
}

func (publication Publication) metric(variant, name string) (float64, bool) {
	for _, result := range publication.Results {
		if result.Variant == variant && result.Name == name {
			return median(result.NsPerOp), true
		}
	}
	return 0, false
}

func (publication Publication) crosslangMetric(implementation, corpus string) (CrosslangResult, bool) {
	for _, result := range publication.Crosslang {
		if result.Implementation == implementation && result.Corpus == corpus {
			return result, true
		}
	}
	return CrosslangResult{}, false
}

func median(values []float64) float64 {
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 != 0 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}

func geomean(values []float64) float64 {
	sum := 0.0
	for _, value := range values {
		sum += math.Log(value)
	}
	return math.Exp(sum / float64(len(values)))
}

func simdChartSummary(rows []simdChartRow) string {
	var out strings.Builder
	out.WriteString("Data: ")
	for i, row := range rows {
		if i > 0 {
			out.WriteString("; ")
		}
		fmt.Fprintf(&out, "%s %.3fx (%d/7 wins)", row.label, row.ratio, row.wins)
	}
	out.WriteByte('.')
	return out.String()
}

func goChartSummary(rows []goChartSeries) string {
	var out strings.Builder
	out.WriteString("Data (operation=value): ")
	for rowIndex, row := range rows {
		if rowIndex > 0 {
			out.WriteString("; ")
		}
		out.WriteString(row.label)
		for operationIndex, operation := range goChartOperations {
			out.WriteByte(' ')
			out.WriteString(operation.Group)
			out.WriteByte('=')
			switch {
			case row.kind == "stable" && row.valid[operationIndex][0]:
				fmt.Fprintf(&out, "%.2fx stable", row.values[operationIndex][0])
			case row.kind != "stable" && row.valid[operationIndex][0] && row.valid[operationIndex][1]:
				fmt.Fprintf(&out, "%.2fx/%.2fx", row.values[operationIndex][0], row.values[operationIndex][1])
			default:
				out.WriteString("not measured")
			}
		}
	}
	out.WriteByte('.')
	return out.String()
}

func crosslangChartSummary(publication Publication) string {
	var out strings.Builder
	out.WriteString("Data (C++/Go portable/Go SIMD): ")
	for i, corpus := range corpusOrder {
		if i > 0 {
			out.WriteString("; ")
		}
		cpp, _ := publication.crosslangMetric("cpp", corpus)
		pure, _ := publication.crosslangMetric("go-pure", corpus)
		simd, _ := publication.crosslangMetric("go-simd", corpus)
		fmt.Fprintf(&out, "%s %s/%s/%s, digest %s", corpusLabel(corpus), formatCompactDuration(cpp.NsPerOp), formatCompactDuration(pure.NsPerOp), formatCompactDuration(simd.NsPerOp), cpp.Digest[:8])
	}
	out.WriteByte('.')
	return out.String()
}

func formatRatioTick(value float64) string {
	label := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", value), "0"), ".")
	return label + "x"
}

func chartProvenance(publication Publication) string {
	return fmt.Sprintf("%s · %s · %s/%s · %d×%s · Go %s",
		publication.Metadata.Commit[:8], publication.Metadata.Machine,
		publication.Metadata.OS, publication.Metadata.Arch,
		publication.Metadata.Samples, publication.Metadata.BenchTime,
		publication.Metadata.GoCommit[:8])
}

func crosslangProvenance(publication Publication) string {
	return fmt.Sprintf("%s · %s · %s/%s · median of %d calibrated samples (≥%s each) · Go %s",
		publication.Metadata.Commit[:8], publication.Metadata.Machine,
		publication.Metadata.OS, publication.Metadata.Arch,
		publication.Metadata.CrossSamples, publication.Metadata.CrossMinTime,
		publication.Metadata.GoCommit[:8])
}

func corpusLabel(corpus string) string {
	labels := map[string]string{
		"canada_geometry": "Canada",
		"citm_catalog":    "CITM",
		"golang_source":   "Go source",
		"string_escaped":  "Escaped",
		"string_unicode":  "Unicode",
		"synthea_fhir":    "Synthea",
		"twitter_status":  "Twitter",
	}
	return labels[corpus]
}

func formatCompactDuration(ns float64) string {
	switch {
	case ns < 1e3:
		return fmt.Sprintf("%.0f ns", ns)
	case ns < 1e5:
		return fmt.Sprintf("%.1f us", ns/1e3)
	case ns < 1e6:
		return fmt.Sprintf("%.0f us", ns/1e3)
	default:
		return fmt.Sprintf("%.2f ms", ns/1e6)
	}
}
