package benchmarks

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thesyncim/vibejson"
	stdlibcorpus "github.com/thesyncim/vibejson/tests/stdlib"
)

type benchmarkCorpus struct {
	label string
	src   []byte
}

// corpusBenchModel keeps every benchmark family on the same concrete corpus
// model. This prevents the comparison, typed decode, and encode harnesses from
// drifting into different type selections.
type corpusBenchModel struct {
	comparison func(*testing.B, []byte)
	typed      func(*testing.B, []byte, vibejson.DecoderOptions)
	encode     func(*testing.B, []byte)
}

var corpusBenchModels = map[string]corpusBenchModel{
	"canada_geometry.json.zst": corpusBenchModelFor[stdlibcorpus.CanadaRoot](),
	"citm_catalog.json.zst":    corpusBenchModelFor[stdlibcorpus.CITMRoot](),
	"golang_source.json.zst":   corpusBenchModelFor[stdlibcorpus.GolangRoot](),
	"string_escaped.json.zst":  corpusBenchModelFor[stdlibcorpus.StringRoot](),
	"string_unicode.json.zst":  corpusBenchModelFor[stdlibcorpus.StringRoot](),
	"synthea_fhir.json.zst":    corpusBenchModelFor[stdlibcorpus.SyntheaRoot](),
	"twitter_status.json.zst":  corpusBenchModelFor[stdlibcorpus.TwitterRoot](),
}

func corpusBenchModelFor[T any]() corpusBenchModel {
	return corpusBenchModel{
		comparison: benchmarkComparison[T],
		typed:      benchmarkCorpusTyped[T],
		encode:     benchmarkCorpusEncode[T],
	}
}

func benchmarkModel(b *testing.B, name, kind string) corpusBenchModel {
	model, ok := corpusBenchModels[name]
	if !ok {
		b.Fatalf("missing %s corpus model for %s", kind, name)
	}
	return model
}

func loadBenchmarkCorpora(tb testing.TB) []benchmarkCorpus {
	tb.Helper()
	out := make([]benchmarkCorpus, 0, len(stdlibcorpus.Names))
	for _, name := range stdlibcorpus.Names {
		src, err := stdlibcorpus.Read(name)
		if err != nil {
			tb.Fatal(err)
		}
		if !json.Valid(src) {
			tb.Fatalf("%s: corpus document is not valid JSON", name)
		}
		out = append(out, benchmarkCorpus{
			label: strings.TrimSuffix(name, ".json.zst"),
			src:   src,
		})
	}
	return out
}

func reportPerPosition(b *testing.B, positions int) {
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(positions), "ns/pos")
}
