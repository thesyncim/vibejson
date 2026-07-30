// Command benchchart turns repeated comparison benchmark output into a compact
// machine-readable snapshot and deterministic SVG charts.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	stdlibcorpus "github.com/thesyncim/vibejson/tests/stdlib"
)

var corpusOrder = []string{
	"canada_geometry",
	"citm_catalog",
	"golang_source",
	"string_escaped",
	"string_unicode",
	"synthea_fhir",
	"twitter_status",
}

var operations = []operationSpec{
	{id: "validate", label: "Strict validation · valid input"},
	{id: "decode-typed-owned", label: "Typed owned decode · reused destination"},
	{id: "decode-dynamic-owned", label: "Dynamic owned decode · new any tree"},
	{id: "encode-owned", label: "Typed encode · newly owned output"},
}

var seriesOrder = []seriesSpec{
	{id: "vibejson/portable", label: "vibejson portable", library: "vibejson", mode: "portable", class: "portable"},
	{id: "vibejson/simd", label: "vibejson SIMD", library: "vibejson", mode: "simd", class: "simd"},
	{id: "encoding-json/portable", label: "encoding/json", library: "encoding-json", mode: "portable", class: "reference"},
	{id: "go-json/portable", label: "go-json", library: "go-json", mode: "portable", class: "peer"},
	{id: "segmentio/portable", label: "segmentio", library: "segmentio", mode: "portable", class: "peer"},
	{id: "jsoniter/portable", label: "jsoniter", library: "jsoniter", mode: "portable", class: "peer"},
}

var numericWorkloads = []numericWorkloadSpec{
	{id: "identifiers", label: "1,024 positive 16-digit identifiers"},
	{id: "telemetry", label: "32,768 fixed-precision telemetry samples"},
	{id: "coordinates", label: "32,768 long geographic coordinates"},
}

var numericSeriesOrder = []seriesSpec{
	{id: "vibejson/portable", label: "vibejson portable", library: "vibejson", mode: "portable", class: "portable"},
	{id: "vibejson/simd", label: "vibejson SIMD", library: "vibejson", mode: "simd", class: "simd"},
	{id: "encoding-json/portable", label: "encoding/json", library: "encoding-json", mode: "portable", class: "reference"},
}

type operationSpec struct {
	id    string
	label string
}

type numericWorkloadSpec struct {
	id    string
	label string
}

type seriesSpec struct {
	id      string
	label   string
	library string
	mode    string
	class   string
}

type metricKey struct {
	mode      string
	corpus    string
	operation string
	library   string
}

type sample struct {
	nsPerOp     float64
	bytesPerOp  float64
	allocsPerOp float64
}

type Metadata struct {
	Commit             string            `json:"commit"`
	GoVersion          string            `json:"go_version"`
	Machine            string            `json:"machine"`
	OS                 string            `json:"os"`
	Arch               string            `json:"arch"`
	Samples            int               `json:"samples"`
	BenchTime          string            `json:"bench_time"`
	CPU                int               `json:"cpu"`
	CorpusBytes        int               `json:"corpus_bytes"`
	CompetitorVersions map[string]string `json:"competitor_versions"`
	Commands           []string          `json:"commands"`
}

type Result struct {
	Mode        string  `json:"mode"`
	Corpus      string  `json:"corpus"`
	Operation   string  `json:"operation"`
	Library     string  `json:"library"`
	NsPerOp     float64 `json:"ns_per_op"`
	BytesPerOp  float64 `json:"bytes_per_op"`
	AllocsPerOp float64 `json:"allocs_per_op"`
}

type Aggregate struct {
	Operation     string  `json:"operation"`
	Series        string  `json:"series"`
	NsPerPass     float64 `json:"ns_per_seven_file_pass"`
	BytesPerPass  float64 `json:"bytes_per_seven_file_pass"`
	AllocsPerPass float64 `json:"allocs_per_seven_file_pass"`
}

type Publication struct {
	Metadata   Metadata    `json:"metadata"`
	Results    []Result    `json:"results"`
	Aggregates []Aggregate `json:"aggregates"`
}

type numericMetricKey struct {
	mode     string
	workload string
	library  string
}

type numericSample struct {
	nsPerOp     float64
	bytesPerOp  float64
	allocsPerOp float64
	inputBytes  float64
	values      float64
}

type NumericResult struct {
	Mode        string  `json:"mode"`
	Workload    string  `json:"workload"`
	Library     string  `json:"library"`
	NsPerOp     float64 `json:"ns_per_op"`
	BytesPerOp  float64 `json:"bytes_per_op"`
	AllocsPerOp float64 `json:"allocs_per_op"`
	InputBytes  int     `json:"input_bytes"`
	Values      int     `json:"values"`
}

type NumericMetadata struct {
	Commit    string   `json:"commit"`
	GoVersion string   `json:"go_version"`
	Machine   string   `json:"machine"`
	OS        string   `json:"os"`
	Arch      string   `json:"arch"`
	Samples   int      `json:"samples"`
	BenchTime string   `json:"bench_time"`
	CPU       int      `json:"cpu"`
	Commands  []string `json:"commands"`
}

type NumericPublication struct {
	Metadata NumericMetadata `json:"metadata"`
	Results  []NumericResult `json:"results"`
}

func main() {
	var (
		portablePath    = flag.String("portable", "", "portable comparison benchmark output")
		simdPath        = flag.String("simd", "", "SIMD vibejson benchmark output")
		numericPortable = flag.String("numeric-portable", "", "portable numeric benchmark output")
		numericSIMD     = flag.String("numeric-simd", "", "SIMD numeric benchmark output")
		jsonPath        = flag.String("json", "", "output comparison JSON path")
		numericJSONPath = flag.String("numeric-json", "", "output numeric JSON path")
		timeChart       = flag.String("time-chart", "", "output time SVG path")
		bytesChart      = flag.String("bytes-chart", "", "output allocated-bytes SVG path")
		simdChart       = flag.String("simd-chart", "", "output strict-validation SVG path")
		numericChart    = flag.String("numeric-chart", "", "output numeric SIMD SVG path")
		commit          = flag.String("commit", "", "measured repository commit")
		goVersion       = flag.String("go-version", "", "full go version output")
		machine         = flag.String("machine", "", "CPU or machine description")
		goos            = flag.String("os", "", "benchmark operating system")
		goarch          = flag.String("arch", "", "benchmark architecture")
		sampleCount     = flag.Int("samples", 6, "required samples per benchmark row")
		benchTime       = flag.String("benchtime", "300ms", "time target per sample")
	)
	flag.Parse()
	if *portablePath == "" || *simdPath == "" || *jsonPath == "" ||
		*numericPortable == "" || *numericSIMD == "" || *numericJSONPath == "" ||
		*timeChart == "" || *bytesChart == "" || *simdChart == "" || *numericChart == "" || *commit == "" ||
		*goVersion == "" || *machine == "" || *goos == "" || *goarch == "" {
		flag.Usage()
		os.Exit(2)
	}

	all := make(map[metricKey][]sample)
	if err := parseBenchmarkFile(*portablePath, "portable", all); err != nil {
		fatal(err)
	}
	if err := parseBenchmarkFile(*simdPath, "simd", all); err != nil {
		fatal(err)
	}
	corpusBytes, err := totalCorpusBytes()
	if err != nil {
		fatal(err)
	}
	metadata := Metadata{
		Commit:      *commit,
		GoVersion:   *goVersion,
		Machine:     *machine,
		OS:          *goos,
		Arch:        *goarch,
		Samples:     *sampleCount,
		BenchTime:   *benchTime,
		CPU:         1,
		CorpusBytes: corpusBytes,
		CompetitorVersions: map[string]string{
			"go-json":   "v0.10.6",
			"jsoniter":  "v1.1.12",
			"segmentio": "v0.5.4",
		},
		Commands: []string{
			fmt.Sprintf(`GOEXPERIMENT= GOMAXPROCS=1 go test -run '^$' -bench '^BenchmarkComparisonCorpus$' -benchmem -benchtime=%s -count=%d -cpu=1`, *benchTime, *sampleCount),
			fmt.Sprintf(`GOEXPERIMENT=simd GOMAXPROCS=1 go test -run '^$' -bench '^BenchmarkComparisonCorpus$/^.*$/^(validate|decode-typed-owned|decode-dynamic-owned|encode-owned)$/^vibejson$' -benchmem -benchtime=%s -count=%d -cpu=1`, *benchTime, *sampleCount),
		},
	}
	publication, err := buildPublication(all, metadata, *sampleCount)
	if err != nil {
		fatal(err)
	}
	if err := writeJSON(*jsonPath, publication); err != nil {
		fatal(err)
	}
	if err := writeFile(*timeChart, renderChart(publication, chartTime)); err != nil {
		fatal(err)
	}
	if err := writeFile(*bytesChart, renderChart(publication, chartBytes)); err != nil {
		fatal(err)
	}
	if err := writeFile(*simdChart, renderSIMDChart(publication)); err != nil {
		fatal(err)
	}

	numericSamples := make(map[numericMetricKey][]numericSample)
	if err := parseNumericBenchmarkFile(*numericPortable, "portable", numericSamples); err != nil {
		fatal(err)
	}
	if err := parseNumericBenchmarkFile(*numericSIMD, "simd", numericSamples); err != nil {
		fatal(err)
	}
	numericPublication, err := buildNumericPublication(numericSamples, NumericMetadata{
		Commit: *commit, GoVersion: *goVersion, Machine: *machine,
		OS: *goos, Arch: *goarch, Samples: *sampleCount,
		BenchTime: *benchTime, CPU: 1,
		Commands: []string{
			fmt.Sprintf(`GOEXPERIMENT= GOMAXPROCS=1 go test -run '^$' -bench '^BenchmarkNumericDecodePublication$' -benchmem -benchtime=%s -count=%d -cpu=1`, *benchTime, *sampleCount),
			fmt.Sprintf(`GOEXPERIMENT=simd GOMAXPROCS=1 go test -run '^$' -bench '^BenchmarkNumericDecodePublication$' -benchmem -benchtime=%s -count=%d -cpu=1`, *benchTime, *sampleCount),
		},
	}, *sampleCount)
	if err != nil {
		fatal(err)
	}
	if err := writeJSON(*numericJSONPath, numericPublication); err != nil {
		fatal(err)
	}
	if err := writeFile(*numericChart, renderNumericChart(numericPublication)); err != nil {
		fatal(err)
	}
}

func parseBenchmarkFile(path, mode string, dst map[metricKey][]sample) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "BenchmarkComparisonCorpus/") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			return fmt.Errorf("%s: malformed benchmark line %q", path, line)
		}
		name := strings.TrimPrefix(fields[0], "BenchmarkComparisonCorpus/")
		if dash := strings.LastIndexByte(name, '-'); dash >= 0 {
			if _, err := strconv.Atoi(name[dash+1:]); err == nil {
				name = name[:dash]
			}
		}
		parts := strings.Split(name, "/")
		if len(parts) != 3 {
			return fmt.Errorf("%s: unexpected benchmark name %q", path, name)
		}
		ns, ok := precedingMetric(fields, "ns/op")
		if !ok {
			return fmt.Errorf("%s: %s has no ns/op", path, name)
		}
		bytes, ok := precedingMetric(fields, "B/op")
		if !ok {
			return fmt.Errorf("%s: %s has no B/op", path, name)
		}
		allocs, ok := precedingMetric(fields, "allocs/op")
		if !ok {
			return fmt.Errorf("%s: %s has no allocs/op", path, name)
		}
		key := metricKey{mode: mode, corpus: parts[0], operation: parts[1], library: parts[2]}
		dst[key] = append(dst[key], sample{nsPerOp: ns, bytesPerOp: bytes, allocsPerOp: allocs})
	}
	return scanner.Err()
}

func parseNumericBenchmarkFile(path, mode string, dst map[numericMetricKey][]numericSample) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "BenchmarkNumericDecodePublication/") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			return fmt.Errorf("%s: malformed benchmark line %q", path, line)
		}
		name := strings.TrimPrefix(fields[0], "BenchmarkNumericDecodePublication/")
		if dash := strings.LastIndexByte(name, '-'); dash >= 0 {
			if _, err := strconv.Atoi(name[dash+1:]); err == nil {
				name = name[:dash]
			}
		}
		parts := strings.Split(name, "/")
		if len(parts) != 2 {
			return fmt.Errorf("%s: unexpected numeric benchmark name %q", path, name)
		}
		ns, ok := precedingMetric(fields, "ns/op")
		if !ok {
			return fmt.Errorf("%s: %s has no ns/op", path, name)
		}
		bytes, ok := precedingMetric(fields, "B/op")
		if !ok {
			return fmt.Errorf("%s: %s has no B/op", path, name)
		}
		allocs, ok := precedingMetric(fields, "allocs/op")
		if !ok {
			return fmt.Errorf("%s: %s has no allocs/op", path, name)
		}
		inputBytes, ok := precedingMetric(fields, "input-B/op")
		if !ok {
			return fmt.Errorf("%s: %s has no input-B/op", path, name)
		}
		values, ok := precedingMetric(fields, "values/op")
		if !ok {
			return fmt.Errorf("%s: %s has no values/op", path, name)
		}
		key := numericMetricKey{mode: mode, workload: parts[0], library: parts[1]}
		dst[key] = append(dst[key], numericSample{
			nsPerOp: ns, bytesPerOp: bytes, allocsPerOp: allocs,
			inputBytes: inputBytes, values: values,
		})
	}
	return scanner.Err()
}

func precedingMetric(fields []string, unit string) (float64, bool) {
	for i := 1; i < len(fields); i++ {
		if fields[i] != unit || i == 0 {
			continue
		}
		value, err := strconv.ParseFloat(fields[i-1], 64)
		return value, err == nil
	}
	return 0, false
}

func buildPublication(samples map[metricKey][]sample, metadata Metadata, sampleCount int) (Publication, error) {
	publication := Publication{Metadata: metadata}
	for _, operation := range operations {
		for _, corpus := range corpusOrder {
			for _, series := range seriesForOperation(operation.id) {
				key := metricKey{
					mode: series.mode, corpus: corpus,
					operation: operation.id, library: series.library,
				}
				values := samples[key]
				if len(values) != sampleCount {
					return Publication{}, fmt.Errorf("%s/%s: %s has %d samples, want %d",
						corpus, operation.id, series.id, len(values), sampleCount)
				}
				publication.Results = append(publication.Results, Result{
					Mode: series.mode, Corpus: corpus, Operation: operation.id,
					Library:     series.library,
					NsPerOp:     median(values, func(value sample) float64 { return value.nsPerOp }),
					BytesPerOp:  median(values, func(value sample) float64 { return value.bytesPerOp }),
					AllocsPerOp: median(values, func(value sample) float64 { return value.allocsPerOp }),
				})
			}
		}
		for _, series := range seriesForOperation(operation.id) {
			aggregate := Aggregate{Operation: operation.id, Series: series.id}
			for _, result := range publication.Results {
				if result.Operation == operation.id &&
					result.Library == series.library && result.Mode == series.mode {
					aggregate.NsPerPass += result.NsPerOp
					aggregate.BytesPerPass += result.BytesPerOp
					aggregate.AllocsPerPass += result.AllocsPerOp
				}
			}
			publication.Aggregates = append(publication.Aggregates, aggregate)
		}
	}
	return publication, nil
}

func buildNumericPublication(samples map[numericMetricKey][]numericSample, metadata NumericMetadata, sampleCount int) (NumericPublication, error) {
	publication := NumericPublication{Metadata: metadata}
	for _, workload := range numericWorkloads {
		expectedInputBytes := -1
		expectedValues := -1
		for _, series := range numericSeriesOrder {
			key := numericMetricKey{
				mode: series.mode, workload: workload.id, library: series.library,
			}
			values := samples[key]
			if len(values) != sampleCount {
				return NumericPublication{}, fmt.Errorf("%s: %s has %d numeric samples, want %d",
					workload.id, series.id, len(values), sampleCount)
			}
			inputBytes := median(values, func(value numericSample) float64 { return value.inputBytes })
			valueCount := median(values, func(value numericSample) float64 { return value.values })
			if inputBytes != math.Trunc(inputBytes) || valueCount != math.Trunc(valueCount) {
				return NumericPublication{}, fmt.Errorf("%s: non-integral benchmark shape", workload.id)
			}
			result := NumericResult{
				Mode: series.mode, Workload: workload.id, Library: series.library,
				NsPerOp:     median(values, func(value numericSample) float64 { return value.nsPerOp }),
				BytesPerOp:  median(values, func(value numericSample) float64 { return value.bytesPerOp }),
				AllocsPerOp: median(values, func(value numericSample) float64 { return value.allocsPerOp }),
				InputBytes:  int(inputBytes),
				Values:      int(valueCount),
			}
			if expectedInputBytes < 0 {
				expectedInputBytes, expectedValues = result.InputBytes, result.Values
			} else if result.InputBytes != expectedInputBytes || result.Values != expectedValues {
				return NumericPublication{}, fmt.Errorf("%s: %s shape is %d bytes/%d values, want %d/%d",
					workload.id, series.id, result.InputBytes, result.Values, expectedInputBytes, expectedValues)
			}
			if result.BytesPerOp != 0 || result.AllocsPerOp != 0 {
				return NumericPublication{}, fmt.Errorf("%s: %s allocates %.0f B/op in %.0f allocs/op",
					workload.id, series.id, result.BytesPerOp, result.AllocsPerOp)
			}
			publication.Results = append(publication.Results, result)
		}
	}
	return publication, nil
}

func median[T any](values []T, metric func(T) float64) float64 {
	numbers := make([]float64, len(values))
	for i, value := range values {
		numbers[i] = metric(value)
	}
	slices.Sort(numbers)
	mid := len(numbers) / 2
	if len(numbers)%2 != 0 {
		return numbers[mid]
	}
	return (numbers[mid-1] + numbers[mid]) / 2
}

func totalCorpusBytes() (int, error) {
	total := 0
	for _, name := range stdlibcorpus.Names {
		src, err := stdlibcorpus.Read(name)
		if err != nil {
			return 0, err
		}
		total += len(src)
	}
	return total, nil
}

func writeJSON(path string, publication any) error {
	data, err := json.MarshalIndent(publication, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFile(path, data)
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

type chartKind uint8

const (
	chartTime chartKind = iota
	chartBytes
)

func renderChart(publication Publication, kind chartKind) []byte {
	const (
		width       = 1100
		top         = 92.0
		panelHeight = 254.0
		labelWidth  = 174.0
		plotWidth   = 690.0
	)
	height := int(top + panelHeight*float64(len(operations)) + 28)
	title := "Absolute time for one seven-file corpus pass"
	subtitle := fmt.Sprintf("Sum of %d-sample per-file medians · same input, ownership, compiler and CPU · lower is better", publication.Metadata.Samples)
	if kind == chartBytes {
		title = "Absolute heap bytes for one seven-file corpus pass"
		subtitle = "Sum of median B/op across seven files · contract-matched public operations · lower is better"
	}
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title desc">`, width, height, width, height)
	fmt.Fprintf(&out, `<title id="title">%s</title>`, html.EscapeString(title))
	fmt.Fprintf(&out, `<desc id="desc">%s. Measured on %s at commit %s.</desc>`, html.EscapeString(subtitle), html.EscapeString(publication.Metadata.Machine), html.EscapeString(shortCommit(publication.Metadata.Commit)))
	out.WriteString(`<style>
text{fill:#24292f;font:14px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}.heading{font-size:21px;font-weight:600}.panel{font-size:16px;font-weight:600}.note{font-size:12px;fill:#57606a}.grid{stroke:#d0d7de;stroke-width:1}.portable{fill:#8250df}.simd{fill:#0969da}.reference{fill:#6e7781}.peer{fill:#1a7f37}@media(prefers-color-scheme:dark){text{fill:#f0f6fc}.note{fill:#8c959f}.grid{stroke:#30363d}.portable{fill:#bc8cff}.simd{fill:#58a6ff}.reference{fill:#8c959f}.peer{fill:#3fb950}}</style>`)
	fmt.Fprintf(&out, `<text class="heading" x="14" y="28">%s</text>`, html.EscapeString(title))
	fmt.Fprintf(&out, `<text class="note" x="14" y="50">%s</text>`, html.EscapeString(subtitle))
	fmt.Fprintf(&out, `<text class="note" x="14" y="69">%s · %s/%s · %s · commit %s</text>`, html.EscapeString(publication.Metadata.Machine), html.EscapeString(publication.Metadata.OS), html.EscapeString(publication.Metadata.Arch), html.EscapeString(shortGoVersion(publication.Metadata.GoVersion)), html.EscapeString(shortCommit(publication.Metadata.Commit)))

	for operationIndex, operation := range operations {
		panelY := top + float64(operationIndex)*panelHeight
		plotLeft := 14.0 + labelWidth
		operationSeries := seriesForOperation(operation.id)
		values := make([]float64, len(operationSeries))
		maxValue := 0.0
		for i, series := range operationSeries {
			aggregate := findAggregate(publication, operation.id, series.id)
			if kind == chartTime {
				values[i] = aggregate.NsPerPass
			} else {
				values[i] = aggregate.BytesPerPass
			}
			maxValue = math.Max(maxValue, values[i])
		}
		scaleMax := niceMaximum(maxValue)
		fmt.Fprintf(&out, `<text class="panel" x="14" y="%.1f">%s</text>`, panelY+17, html.EscapeString(operation.label))
		for tick := 0; tick <= 2; tick++ {
			value := scaleMax * float64(tick) / 2
			x := plotLeft + plotWidth*float64(tick)/2
			fmt.Fprintf(&out, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/><text class="note" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`,
				x, panelY+28, x, panelY+226, x, panelY+25, html.EscapeString(formatMetric(value, kind)))
		}
		for i, series := range operationSeries {
			y := panelY + 43 + float64(i)*30
			barWidth := math.Max(1, values[i]/scaleMax*plotWidth)
			fmt.Fprintf(&out, `<text x="14" y="%.1f">%s</text><rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="16" rx="2"/><text x="%.1f" y="%.1f">%s</text>`,
				y+13, html.EscapeString(series.label), series.class, plotLeft, y, barWidth, plotLeft+barWidth+7, y+13, html.EscapeString(formatMetric(values[i], kind)))
		}
	}
	fmt.Fprintf(&out, `<text class="note" x="14" y="%d">Source: benchmarks/results/comparison.json · full per-file time, B/op and allocs/op are retained there.</text>`, height-9)
	out.WriteString(`</svg>`)
	return append([]byte(out.String()), '\n')
}

func renderSIMDChart(publication Publication) []byte {
	const (
		width       = 1100
		top         = 96.0
		panelHeight = 248.0
		labelWidth  = 174.0
		plotWidth   = 690.0
	)
	panels := []struct {
		label   string
		corpora []string
	}{
		{label: "All seven real corpus files"},
		{label: "String-rich payloads · escaped + Unicode files", corpora: []string{"string_escaped", "string_unicode"}},
	}
	height := int(top + panelHeight*float64(len(panels)) + 30)
	title := "Strict whole-document validation"
	subtitle := fmt.Sprintf("Absolute time from %d-sample per-file medians · valid input · lower is better", publication.Metadata.Samples)
	validationSeries := seriesForOperation("validate")

	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title desc">`, width, height, width, height)
	fmt.Fprintf(&out, `<title id="title">%s</title>`, html.EscapeString(title))
	fmt.Fprintf(&out, `<desc id="desc">%s. Measured on %s at commit %s.</desc>`, html.EscapeString(subtitle), html.EscapeString(publication.Metadata.Machine), html.EscapeString(shortCommit(publication.Metadata.Commit)))
	out.WriteString(`<style>
text{fill:#24292f;font:14px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}.heading{font-size:21px;font-weight:600}.panel{font-size:16px;font-weight:600}.note{font-size:12px;fill:#57606a}.gain{font-size:13px;fill:#0969da;font-weight:600}.grid{stroke:#d0d7de;stroke-width:1}.portable{fill:#8250df}.simd{fill:#0969da}.reference{fill:#6e7781}.peer{fill:#1a7f37}@media(prefers-color-scheme:dark){text{fill:#f0f6fc}.note{fill:#8c959f}.gain{fill:#58a6ff}.grid{stroke:#30363d}.portable{fill:#bc8cff}.simd{fill:#58a6ff}.reference{fill:#8c959f}.peer{fill:#3fb950}}</style>`)
	fmt.Fprintf(&out, `<text class="heading" x="14" y="28">%s</text>`, html.EscapeString(title))
	fmt.Fprintf(&out, `<text class="note" x="14" y="50">%s</text>`, html.EscapeString(subtitle))
	fmt.Fprintf(&out, `<text class="note" x="14" y="69">%s · %s/%s · %s · commit %s</text>`, html.EscapeString(publication.Metadata.Machine), html.EscapeString(publication.Metadata.OS), html.EscapeString(publication.Metadata.Arch), html.EscapeString(shortGoVersion(publication.Metadata.GoVersion)), html.EscapeString(shortCommit(publication.Metadata.Commit)))

	for panelIndex, panel := range panels {
		panelY := top + float64(panelIndex)*panelHeight
		plotLeft := 14.0 + labelWidth
		values := make([]float64, len(validationSeries))
		maxValue := 0.0
		for i, series := range validationSeries {
			values[i] = validationTime(publication, series, panel.corpora)
			maxValue = math.Max(maxValue, values[i])
		}
		peerBest := math.MaxFloat64
		for i, series := range validationSeries {
			if series.library != "vibejson" {
				peerBest = math.Min(peerBest, values[i])
			}
		}
		simdTime := validationTime(publication, seriesOrder[1], panel.corpora)
		portableTime := validationTime(publication, seriesOrder[0], panel.corpora)
		scaleMax := niceMaximum(maxValue)
		fmt.Fprintf(&out, `<text class="panel" x="14" y="%.1f">%s</text>`, panelY+17, html.EscapeString(panel.label))
		fmt.Fprintf(&out, `<text class="gain" x="14" y="%.1f">SIMD: %.1f× faster than fastest strict peer · %.1f× faster than portable</text>`,
			panelY+38, peerBest/simdTime, portableTime/simdTime)
		for tick := 0; tick <= 2; tick++ {
			value := scaleMax * float64(tick) / 2
			x := plotLeft + plotWidth*float64(tick)/2
			fmt.Fprintf(&out, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/><text class="note" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`,
				x, panelY+48, x, panelY+220, x, panelY+45, html.EscapeString(formatMetric(value, chartTime)))
		}
		for i, series := range validationSeries {
			y := panelY + 62 + float64(i)*30
			barWidth := math.Max(1, values[i]/scaleMax*plotWidth)
			fmt.Fprintf(&out, `<text x="14" y="%.1f">%s</text><rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="16" rx="2"/><text x="%.1f" y="%.1f">%s</text>`,
				y+13, html.EscapeString(series.label), series.class, plotLeft, y, barWidth, plotLeft+barWidth+7, y+13, html.EscapeString(formatMetric(values[i], chartTime)))
		}
	}
	fmt.Fprintf(&out, `<text class="note" x="14" y="%d">jsoniter is omitted here: its Valid API accepts trailing non-space bytes. All included validators reject the checked late-invalid variant.</text>`, height-24)
	fmt.Fprintf(&out, `<text class="note" x="14" y="%d">Source: benchmarks/results/comparison.json · every bar starts at zero.</text>`, height-9)
	out.WriteString(`</svg>`)
	return append([]byte(out.String()), '\n')
}

func renderNumericChart(publication NumericPublication) []byte {
	const (
		width       = 1100
		top         = 96.0
		panelHeight = 190.0
		labelWidth  = 174.0
		plotWidth   = 690.0
	)
	height := int(top + panelHeight*float64(len(numericWorkloads)) + 30)
	title := "Reused typed decode · numeric-heavy JSON arrays"
	subtitle := fmt.Sprintf("Absolute time from %d-sample medians · complete public Decode calls · lower is better", publication.Metadata.Samples)

	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title desc">`, width, height, width, height)
	fmt.Fprintf(&out, `<title id="title">%s</title>`, html.EscapeString(title))
	fmt.Fprintf(&out, `<desc id="desc">%s. Measured on %s at commit %s.</desc>`, html.EscapeString(subtitle), html.EscapeString(publication.Metadata.Machine), html.EscapeString(shortCommit(publication.Metadata.Commit)))
	out.WriteString(`<style>
text{fill:#24292f;font:14px -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}.heading{font-size:21px;font-weight:600}.panel{font-size:16px;font-weight:600}.note{font-size:12px;fill:#57606a}.gain{font-size:13px;fill:#0969da;font-weight:600}.grid{stroke:#d0d7de;stroke-width:1}.portable{fill:#8250df}.simd{fill:#0969da}.reference{fill:#6e7781}@media(prefers-color-scheme:dark){text{fill:#f0f6fc}.note{fill:#8c959f}.gain{fill:#58a6ff}.grid{stroke:#30363d}.portable{fill:#bc8cff}.simd{fill:#58a6ff}.reference{fill:#8c959f}}</style>`)
	fmt.Fprintf(&out, `<text class="heading" x="14" y="28">%s</text>`, html.EscapeString(title))
	fmt.Fprintf(&out, `<text class="note" x="14" y="50">%s</text>`, html.EscapeString(subtitle))
	fmt.Fprintf(&out, `<text class="note" x="14" y="69">%s · %s/%s · %s · commit %s</text>`, html.EscapeString(publication.Metadata.Machine), html.EscapeString(publication.Metadata.OS), html.EscapeString(publication.Metadata.Arch), html.EscapeString(shortGoVersion(publication.Metadata.GoVersion)), html.EscapeString(shortCommit(publication.Metadata.Commit)))

	for panelIndex, workload := range numericWorkloads {
		panelY := top + float64(panelIndex)*panelHeight
		plotLeft := 14.0 + labelWidth
		values := make([]float64, len(numericSeriesOrder))
		maxValue := 0.0
		for i, series := range numericSeriesOrder {
			values[i] = findNumericResult(publication, workload.id, series).NsPerOp
			maxValue = math.Max(maxValue, values[i])
		}
		portableTime := values[0]
		simdTime := values[1]
		input := findNumericResult(publication, workload.id, numericSeriesOrder[0])
		scaleMax := niceMaximum(maxValue)
		fmt.Fprintf(&out, `<text class="panel" x="14" y="%.1f">%s</text>`, panelY+17, html.EscapeString(workload.label))
		fmt.Fprintf(&out, `<text class="gain" x="14" y="%.1f">SIMD: %.2f× faster than portable · %s input · %d values · %.0f B/op · %.0f allocs/op</text>`,
			panelY+38, portableTime/simdTime, formatInputBytes(input.InputBytes), input.Values, input.BytesPerOp, input.AllocsPerOp)
		for tick := 0; tick <= 2; tick++ {
			value := scaleMax * float64(tick) / 2
			x := plotLeft + plotWidth*float64(tick)/2
			fmt.Fprintf(&out, `<line class="grid" x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f"/><text class="note" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`,
				x, panelY+48, x, panelY+159, x, panelY+45, html.EscapeString(formatMetric(value, chartTime)))
		}
		for i, series := range numericSeriesOrder {
			y := panelY + 62 + float64(i)*30
			barWidth := math.Max(1, values[i]/scaleMax*plotWidth)
			fmt.Fprintf(&out, `<text x="14" y="%.1f">%s</text><rect class="%s" x="%.1f" y="%.1f" width="%.1f" height="16" rx="2"/><text x="%.1f" y="%.1f">%s</text>`,
				y+13, html.EscapeString(series.label), series.class, plotLeft, y, barWidth, plotLeft+barWidth+7, y+13, html.EscapeString(formatMetric(values[i], chartTime)))
		}
	}
	fmt.Fprintf(&out, `<text class="note" x="14" y="%d">Same JSON bytes, compiler, CPU, reused destination, and complete-document semantics. Every bar starts at zero.</text>`, height-24)
	fmt.Fprintf(&out, `<text class="note" x="14" y="%d">Source: benchmarks/results/numeric.json · time, B/op, allocs/op, input bytes, and value counts are retained there.</text>`, height-9)
	out.WriteString(`</svg>`)
	return append([]byte(out.String()), '\n')
}

func findNumericResult(publication NumericPublication, workload string, series seriesSpec) NumericResult {
	for _, result := range publication.Results {
		if result.Workload == workload && result.Library == series.library && result.Mode == series.mode {
			return result
		}
	}
	panic("missing numeric result")
}

func formatInputBytes(value int) string {
	switch {
	case value >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(value)/(1<<20))
	case value >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(value)/(1<<10))
	default:
		return fmt.Sprintf("%d B", value)
	}
}

func validationTime(publication Publication, series seriesSpec, corpora []string) float64 {
	if len(corpora) == 0 {
		return findAggregate(publication, "validate", series.id).NsPerPass
	}
	total := 0.0
	for _, corpus := range corpora {
		for _, result := range publication.Results {
			if result.Operation == "validate" && result.Corpus == corpus &&
				result.Library == series.library && result.Mode == series.mode {
				total += result.NsPerOp
				break
			}
		}
	}
	return total
}

func seriesForOperation(operation string) []seriesSpec {
	if operation == "validate" {
		// jsoniter.Valid accepts trailing non-space bytes after one value, so it
		// does not implement the strict whole-document contract in this panel.
		return seriesOrder[:len(seriesOrder)-1]
	}
	return seriesOrder
}

func findAggregate(publication Publication, operation, series string) Aggregate {
	for _, aggregate := range publication.Aggregates {
		if aggregate.Operation == operation && aggregate.Series == series {
			return aggregate
		}
	}
	panic("missing aggregate")
}

func niceMaximum(value float64) float64 {
	if value <= 0 {
		return 1
	}
	power := math.Pow(10, math.Floor(math.Log10(value)))
	scaled := value / power
	switch {
	case scaled <= 1:
		return power
	case scaled <= 2:
		return 2 * power
	case scaled <= 5:
		return 5 * power
	default:
		return 10 * power
	}
}

func formatMetric(value float64, kind chartKind) string {
	if kind == chartBytes {
		switch {
		case value >= 1<<20:
			return fmt.Sprintf("%.2f MiB", value/(1<<20))
		case value >= 1<<10:
			return fmt.Sprintf("%.1f KiB", value/(1<<10))
		default:
			return fmt.Sprintf("%.0f B", value)
		}
	}
	switch {
	case value >= 1e6:
		return fmt.Sprintf("%.2f ms", value/1e6)
	case value >= 1e3:
		return fmt.Sprintf("%.1f µs", value/1e3)
	default:
		return fmt.Sprintf("%.0f ns", value)
	}
}

func shortCommit(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

func shortGoVersion(version string) string {
	fields := strings.Fields(version)
	if len(fields) >= 3 && fields[0] == "go" && fields[1] == "version" {
		return fields[2]
	}
	if len(fields) != 0 {
		return fields[0]
	}
	return version
}

func fatal(err error) {
	if !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "benchchart:", err)
	}
	os.Exit(1)
}
