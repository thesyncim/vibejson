package benchmarks

import (
	"encoding/json"
	"strings"
	"testing"

	stdlibcorpus "github.com/thesyncim/simdjson/tests/stdlib"
)

type benchmarkCorpus struct {
	label string
	src   []byte
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
