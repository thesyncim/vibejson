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
text { fill:#24292f; font:15px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
.heading { font-size:20px; font-weight:500; }
.note { font-size:13px; }
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
	label    string
	wins     int
	portable float64
	simd     float64
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
		var portableTimes, simdTimes []float64
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
			portableTimes = append(portableTimes, pure)
			simdTimes = append(simdTimes, simd)
		}
		rows = append(rows, simdChartRow{
			label:    spec.label,
			wins:     wins,
			portable: geomean(portableTimes),
			simd:     geomean(simdTimes),
		})
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
			return p.metric(variant, name)
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
		return p.metric(variant, benchmarkName("BenchmarkStdlibCorpusJSONV2", corpus, operation.Group, "jsonv2"))
	}
	sonic := func(p Publication, _ string, operation benchmarkContract, corpus string) (float64, bool) {
		implementation := sonicImplementation(operation.Group)
		return p.metric("sonic", benchmarkName("BenchmarkStdlibCorpusNativeSonic", corpus, operation.Group, implementation))
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
				var times []float64
				complete := true
				for _, corpus := range corpusOrder {
					value, ok := source.value(publication, mode, operation, corpus)
					if !ok {
						complete = false
						break
					}
					times = append(times, value)
				}
				if complete {
					row.values[operationIndex][modeIndex] = geomean(times)
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
	const margin, gap, top, facetHeight = 16.0, 24.0, 84.0, 174.0
	const columns = 2
	facetWidth := (width - 2*margin - gap*(columns-1)) / columns
	height := int(top + facetHeight*math.Ceil(float64(len(rows))/columns) + 18)
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title desc">`, width, height, width, height)
	out.WriteString(`<title id="title">Portable and SIMD absolute time by operation</title>`)
	fmt.Fprintf(&out, `<desc id="desc">Absolute geometric-mean time per operation over seven corpus files. Each operation has its own zero-based scale; lower is faster. %s Source: benchmarks/results/latest.json. Snapshot %s.</desc>`, html.EscapeString(simdChartSummary(rows)), html.EscapeString(chartProvenance(publication)))
	out.WriteString(chartStyle)
	out.WriteString(`<text class="heading" x="8" y="22">Portable and SIMD time by operation</text>`)
	fmt.Fprintf(&out, `<text class="muted note" x="8" y="43">Absolute geomean time/op across seven payloads · independent scales · lower is faster · %s</text>`, html.EscapeString(chartProvenance(publication)))
	for i, row := range rows {
		column := float64(i % columns)
		line := float64(i / columns)
		facetX := margin + column*(facetWidth+gap)
		facetY := top + line*facetHeight
		plotLeft := facetX + 90
		plotWidth := facetWidth - 194
		scaleMax := niceDurationMax(math.Max(row.portable, row.simd))
		fmt.Fprintf(&out, `<text x="%.1f" y="%.1f">%s</text>`, facetX, facetY+16, html.EscapeString(row.label))
		for tick := 0; tick <= 2; tick++ {
			value := scaleMax * float64(tick) / 2
			x := plotLeft + plotWidth*float64(tick)/2
			fmt.Fprintf(&out, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/><text class="muted note" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x, facetY+34, x, facetY+132, x, facetY+31, html.EscapeString(formatCompactDuration(value)))
		}
		values := []struct {
			label string
			class string
			value float64
		}{
			{label: "portable", class: "portable", value: row.portable},
			{label: "SIMD", class: "simd", value: row.simd},
		}
		for series, item := range values {
			y := facetY + 58 + float64(series)*38
			barWidth := math.Max(1, item.value/scaleMax*plotWidth)
			fmt.Fprintf(&out, `<text class="note" x="%.1f" y="%.1f">%s</text><rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="14" rx="2"/><text class="note" x="%.1f" y="%.1f">%s</text>`, facetX, y+11, item.label, item.class, plotLeft, y, barWidth, plotLeft+barWidth+5, y+11, html.EscapeString(formatCompactDuration(item.value)))
		}
		fmt.Fprintf(&out, `<text class="muted note" x="%.1f" y="%.1f">SIMD lower on %d of 7 payloads</text>`, facetX, facetY+151, row.wins)
	}
	out.WriteString(`</svg>`)
	return append([]byte(out.String()), '\n')
}

func renderGoChart(publication Publication, rows []goChartSeries) []byte {
	const width = 1000
	const margin, gap, top, facetHeight = 16.0, 0.0, 84.0, 390.0
	const columns = 1
	facetWidth := (width - 2*margin - gap*(columns-1)) / columns
	height := int(top + facetHeight*math.Ceil(float64(len(goChartOperations))/columns) + 18)
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title desc">`, width, height, width, height)
	out.WriteString(`<title id="title">Go library absolute time by measured operation</title>`)
	fmt.Fprintf(&out, `<desc id="desc">Absolute geometric-mean time per operation over seven corpus files. Each operation has its own zero-based scale; lower is faster. Regular rows show portable and SIMD compiler modes. %s Source: benchmarks/results/latest.json. Snapshot %s.</desc>`, html.EscapeString(goChartSummary(rows)), html.EscapeString(chartProvenance(publication)))
	out.WriteString(chartStyle)
	out.WriteString(`<text class="heading" x="8" y="22">Go library time by measured operation</text>`)
	fmt.Fprintf(&out, `<text class="muted note" x="8" y="43">Absolute geomean time/op across seven payloads · independent scales · lower is faster · %s</text>`, html.EscapeString(chartProvenance(publication)))
	out.WriteString(`<rect class="portable" x="8" y="55" width="12" height="12" rx="2"/><text class="note" x="26" y="66">portable</text><rect class="simd" x="100" y="55" width="12" height="12" rx="2"/><text class="note" x="118" y="66">SIMD</text><rect class="reference" x="176" y="55" width="12" height="12" rx="2"/><text class="note" x="194" y="66">stable-only context</text>`)
	for operationIndex, operation := range goChartOperations {
		column := float64(operationIndex % columns)
		line := float64(operationIndex / columns)
		facetX := margin + column*(facetWidth+gap)
		facetY := top + line*facetHeight
		plotLeft := facetX + 180
		plotWidth := facetWidth - 300
		maxValue := 0.0
		for _, row := range rows {
			for mode := 0; mode < 2; mode++ {
				if row.valid[operationIndex][mode] {
					maxValue = math.Max(maxValue, row.values[operationIndex][mode])
				}
			}
		}
		scaleMax := niceDurationMax(maxValue)
		fmt.Fprintf(&out, `<text x="%.1f" y="%.1f">%s</text>`, facetX, facetY+16, html.EscapeString(operation.ChartLabel))
		for tick := 0; tick <= 2; tick++ {
			value := scaleMax * float64(tick) / 2
			x := plotLeft + plotWidth*float64(tick)/2
			fmt.Fprintf(&out, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/><text class="muted note" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x, facetY+35, x, facetY+365, x, facetY+31, html.EscapeString(formatCompactDuration(value)))
		}
		for rowIndex, row := range rows {
			y := facetY + 55 + float64(rowIndex)*39
			fmt.Fprintf(&out, `<text class="note" x="%.1f" y="%.1f">%s</text>`, facetX, y+13, html.EscapeString(row.label))
			if row.kind == "stable" {
				if row.valid[operationIndex][0] {
					barWidth := math.Max(1, row.values[operationIndex][0]/scaleMax*plotWidth)
					fmt.Fprintf(&out, `<rect class="reference" x="%.1f" y="%.1f" width="%.1f" height="12" rx="2"/><text class="note" x="%.1f" y="%.1f">%s</text>`, plotLeft, y+3, barWidth, plotLeft+barWidth+6, y+14, html.EscapeString(formatCompactDuration(row.values[operationIndex][0])))
				} else {
					fmt.Fprintf(&out, `<text class="muted" x="%.1f" y="%.1f">—</text>`, plotLeft+6, y+13)
				}
				continue
			}
			if !row.valid[operationIndex][0] && !row.valid[operationIndex][1] {
				fmt.Fprintf(&out, `<text class="muted" x="%.1f" y="%.1f">—</text>`, plotLeft+6, y+13)
				continue
			}
			for mode, class := range []string{"portable", "simd"} {
				if !row.valid[operationIndex][mode] {
					continue
				}
				value := row.values[operationIndex][mode]
				barWidth := math.Max(1, value/scaleMax*plotWidth)
				barY := y - 1 + float64(mode)*14
				fmt.Fprintf(&out, `<rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="9" rx="2"/><text class="note" x="%.1f" y="%.1f">%s</text>`, class, plotLeft, barY, barWidth, plotLeft+barWidth+6, barY+9, html.EscapeString(formatCompactDuration(value)))
			}
		}
	}
	fmt.Fprintf(&out, `<text class="muted note" x="8" y="%d">Scan rows show accepted-input throughput, not rejection parity. JSON/v2 lacks scan; Sonic uses stable Go.</text>`, height-8)
	out.WriteString(`</svg>`)
	return append([]byte(out.String()), '\n')
}

func renderCrosslangChart(publication Publication) []byte {
	const width = 1000
	const margin, gap, top, facetHeight = 16.0, 24.0, 84.0, 190.0
	const columns = 2
	facetWidth := (width - 2*margin - gap*(columns-1)) / columns
	height := int(top + facetHeight*math.Ceil(float64(len(corpusOrder))/columns) + 18)
	var out strings.Builder
	firstCorpus := corpusOrder[0]
	pureBackend, _ := publication.crosslangMetric("go-pure", firstCorpus)
	simdBackend, _ := publication.crosslangMetric("go-simd", firstCorpus)
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title desc">`, width, height, width, height)
	out.WriteString(`<title id="title">C++ and Go parse plus semantic digest</title>`)
	fmt.Fprintf(&out, `<desc id="desc">Absolute completion time for matched semantic digests in C++ simdjson, portable Go, and SIMD Go. Every corpus has its own scale; lower is faster. %s Source: benchmarks/results/latest.json. Snapshot %s; C++ %s at %s.</desc>`, html.EscapeString(crosslangChartSummary(publication)), html.EscapeString(crosslangProvenance(publication)), html.EscapeString(publication.Metadata.CXXLibrary), html.EscapeString(publication.Metadata.CXXCommit))
	out.WriteString(chartStyle)
	out.WriteString(`<text class="heading" x="8" y="22">C++ and Go parse + semantic digest</text>`)
	fmt.Fprintf(&out, `<text class="muted note" x="8" y="43">Identical digest contract · absolute time/op · independent corpus scales · lower is faster · %s</text>`, html.EscapeString(crosslangProvenance(publication)))
	fmt.Fprintf(&out, `<rect class="reference" x="8" y="57" width="12" height="12" rx="2"/><text class="note" x="26" y="68">C++ simdjson</text><rect class="portable" x="140" y="57" width="12" height="12" rx="2"/><text class="note" x="158" y="68">Go portable (%s)</text><rect class="simd" x="438" y="57" width="12" height="12" rx="2"/><text class="note" x="456" y="68">Go SIMD (%s)</text>`, html.EscapeString(pureBackend.Backend), html.EscapeString(simdBackend.Backend))
	for index, corpus := range corpusOrder {
		cpp, _ := publication.crosslangMetric("cpp", corpus)
		pure, _ := publication.crosslangMetric("go-pure", corpus)
		simd, _ := publication.crosslangMetric("go-simd", corpus)
		values := []struct {
			label string
			class string
			value float64
		}{{label: "C++", class: "reference", value: cpp.NsPerOp}, {label: "Go portable", class: "portable", value: pure.NsPerOp}, {label: "Go SIMD", class: "simd", value: simd.NsPerOp}}
		column := float64(index % columns)
		line := float64(index / columns)
		facetX := margin + column*(facetWidth+gap)
		facetY := top + line*facetHeight
		plotLeft := facetX + 92
		plotWidth := facetWidth - 198
		scaleMax := niceDurationMax(math.Max(cpp.NsPerOp, math.Max(pure.NsPerOp, simd.NsPerOp)))
		fmt.Fprintf(&out, `<text x="%.1f" y="%.1f">%s</text>`, facetX, facetY+16, html.EscapeString(corpusLabel(corpus)))
		for tick := 0; tick <= 2; tick++ {
			value := scaleMax * float64(tick) / 2
			x := plotLeft + plotWidth*float64(tick)/2
			fmt.Fprintf(&out, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/><text class="muted note" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x, facetY+35, x, facetY+147, x, facetY+31, html.EscapeString(formatCompactDuration(value)))
		}
		for series, item := range values {
			y := facetY + 53 + float64(series)*34
			barWidth := math.Max(1, item.value/scaleMax*plotWidth)
			fmt.Fprintf(&out, `<text class="note" x="%.1f" y="%.1f">%s</text><rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="14" rx="2"/><text class="note" x="%.1f" y="%.1f">%s</text>`, facetX, y+11, item.label, item.class, plotLeft, y, barWidth, plotLeft+barWidth+5, y+11, html.EscapeString(formatCompactDuration(item.value)))
		}
		fmt.Fprintf(&out, `<text class="muted note" x="%.1f" y="%.1f">digest %s</text>`, facetX, facetY+169, html.EscapeString(cpp.Digest[:8]))
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
	out.WriteString("Data (portable/SIMD absolute time): ")
	for i, row := range rows {
		if i > 0 {
			out.WriteString("; ")
		}
		fmt.Fprintf(&out, "%s %s/%s (%d/7 SIMD wins)", row.label, formatCompactDuration(row.portable), formatCompactDuration(row.simd), row.wins)
	}
	out.WriteByte('.')
	return out.String()
}

func goChartSummary(rows []goChartSeries) string {
	var out strings.Builder
	out.WriteString("Data (operation=portable/SIMD absolute time): ")
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
				fmt.Fprintf(&out, "%s stable", formatCompactDuration(row.values[operationIndex][0]))
			case row.kind != "stable" && row.valid[operationIndex][0] && row.valid[operationIndex][1]:
				fmt.Fprintf(&out, "%s/%s", formatCompactDuration(row.values[operationIndex][0]), formatCompactDuration(row.values[operationIndex][1]))
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

func niceDurationMax(value float64) float64 {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 1
	}
	target := value * 1.04
	magnitude := math.Pow(10, math.Floor(math.Log10(target)))
	fraction := target / magnitude
	for _, step := range []float64{1, 1.25, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10} {
		if fraction <= step {
			return step * magnitude
		}
	}
	return 10 * magnitude
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
		return fmt.Sprintf("%.1f µs", ns/1e3)
	case ns < 1e6:
		return fmt.Sprintf("%.0f µs", ns/1e3)
	default:
		return fmt.Sprintf("%.2f ms", ns/1e6)
	}
}
