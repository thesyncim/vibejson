package benchmarks

import (
	"strings"
	"testing"

	"github.com/thesyncim/simdjson"
	stdlibcorpus "github.com/thesyncim/simdjson/tests/stdlib"
)

// BenchmarkCorpusIndexReused measures reusable structural indexing over the
// repository's pinned real-world corpus.
func BenchmarkCorpusIndexReused(b *testing.B) {
	for _, name := range stdlibcorpus.Names {
		src, err := stdlibcorpus.Read(name)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(strings.TrimSuffix(name, ".json.zst"), func(b *testing.B) {
			need, err := simdjson.RequiredIndexEntries(src)
			if err != nil {
				b.Fatal(err)
			}
			storage := make([]simdjson.IndexEntry, need)
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				index, err := simdjson.BuildIndex(src, storage)
				if err != nil {
					b.Fatal(err)
				}
				intSink = index.Len()
			}
		})
	}
}
