// Single-core scoreboard for field lookup and extraction against the Go
// path-extraction competitors: gjson (github.com/tidwall/gjson), sonic's ast
// (github.com/bytedance/sonic), and encoding/json.
//
// Scenarios:
//
//  1. BenchmarkPointLookupCold: one nested path from one cold document.
//  2. BenchmarkRepeatedLookup16: sixteen paths from one document, amortized.
//  3. BenchmarkCorpusFieldExtract: one field from each of 1024 documents.
//  4. BenchmarkCorpusSumInt64: sum an int64 field across 1024 documents.
//  5. BenchmarkValidateAndIndex: full validation and indexing versus the
//     competitors' parse and validate entry points.
//
// Semantics differ across rows and the comparison is only meaningful with
// that in mind: gjson.GetBytes never validates the document (it scans for the
// path and trusts the rest), sonic's ast validates only along the searched
// path, and BuildIndex/GetRaw validate the entire input. ScanFirstRaw is the
// early-exit spelling: it validates everything before the match and skips the
// tail. ScanFirstRawTrusted is the explicit non-validating spelling with
// gjson's implicit trust contract, so its row is the like-for-like gjson
// comparison. Note also that sonic selects its optimized backend only on
// toolchains it recognizes (go1.17 through go1.26 on amd64/arm64); on newer
// toolchains it silently falls back to a portable path, so sonic rows must be
// read together with the toolchain that produced them.
package benchmarks

import (
	stdjson "encoding/json"
	"fmt"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/tidwall/gjson"

	"github.com/thesyncim/simdjson"
	"github.com/thesyncim/simdjson/document"
	stdlibcorpus "github.com/thesyncim/simdjson/tests/stdlib"
)

var (
	lookupSinkInt   int
	lookupSinkInt64 int64
)

// scoreboardTwitter loads the twitter_status corpus used by the single-document
// scenarios.
func scoreboardTwitter(tb testing.TB) []byte {
	tb.Helper()
	src, err := stdlibcorpus.Read("twitter_status.json.zst")
	if err != nil {
		tb.Fatalf("read twitter corpus: %v", err)
	}
	return src
}

// The point-lookup scenario extracts one nested string from the middle of the
// twitter corpus.
const (
	scoreboardPointPointer = "/statuses/50/user/screen_name"
	scoreboardPointGJSON   = "statuses.50.user.screen_name"
)

var scoreboardPointSonic = []interface{}{"statuses", 50, "user", "screen_name"}

// scoreboardPaths lists the sixteen paths used by the repeated-lookup
// scenario: a spread of scalar fields across the whole document in each
// library's native path spelling.
var scoreboardPaths = []struct {
	pointer string
	gjson   string
	sonic   []interface{}
}{
	{"/statuses/0/id", "statuses.0.id", []interface{}{"statuses", 0, "id"}},
	{"/statuses/0/user/screen_name", "statuses.0.user.screen_name", []interface{}{"statuses", 0, "user", "screen_name"}},
	{"/statuses/10/text", "statuses.10.text", []interface{}{"statuses", 10, "text"}},
	{"/statuses/20/user/followers_count", "statuses.20.user.followers_count", []interface{}{"statuses", 20, "user", "followers_count"}},
	{"/statuses/25/retweet_count", "statuses.25.retweet_count", []interface{}{"statuses", 25, "retweet_count"}},
	{"/statuses/30/user/name", "statuses.30.user.name", []interface{}{"statuses", 30, "user", "name"}},
	{"/statuses/40/created_at", "statuses.40.created_at", []interface{}{"statuses", 40, "created_at"}},
	{"/statuses/50/user/screen_name", "statuses.50.user.screen_name", []interface{}{"statuses", 50, "user", "screen_name"}},
	{"/statuses/55/lang", "statuses.55.lang", []interface{}{"statuses", 55, "lang"}},
	{"/statuses/60/favorite_count", "statuses.60.favorite_count", []interface{}{"statuses", 60, "favorite_count"}},
	{"/statuses/70/user/location", "statuses.70.user.location", []interface{}{"statuses", 70, "user", "location"}},
	{"/statuses/80/source", "statuses.80.source", []interface{}{"statuses", 80, "source"}},
	{"/statuses/90/user/id", "statuses.90.user.id", []interface{}{"statuses", 90, "user", "id"}},
	{"/statuses/99/text", "statuses.99.text", []interface{}{"statuses", 99, "text"}},
	{"/search_metadata/count", "search_metadata.count", []interface{}{"search_metadata", "count"}},
	{"/search_metadata/max_id_str", "search_metadata.max_id_str", []interface{}{"search_metadata", "max_id_str"}},
}

// scoreboardCheckPaths verifies that every library resolves every scoreboard
// path to the same raw token, so the benchmarks compare equal work.
func scoreboardCheckPaths(tb testing.TB, src []byte) {
	tb.Helper()
	for _, p := range scoreboardPaths {
		rv, ok, err := simdjson.GetRaw(src, p.pointer)
		if err != nil || !ok {
			tb.Fatalf("simdjson.GetRaw(%q): ok=%v err=%v", p.pointer, ok, err)
		}
		want := string(rv.Bytes())
		if tv, ok, err := simdjson.ScanFirstRawTrusted(src, p.pointer); err != nil || !ok || string(tv.Bytes()) != want {
			tb.Fatalf("simdjson.ScanFirstRawTrusted(%q) = %q, %v, %v; want %q", p.pointer, tv.Bytes(), ok, err, want)
		}
		if r := gjson.GetBytes(src, p.gjson); !r.Exists() || r.Raw != want {
			tb.Fatalf("gjson.GetBytes(%q) = %q, want %q", p.gjson, r.Raw, want)
		}
		n, err := sonic.Get(src, p.sonic...)
		if err != nil {
			tb.Fatalf("sonic.Get(%v): %v", p.sonic, err)
		}
		raw, err := n.Raw()
		if err != nil || raw != want {
			tb.Fatalf("sonic raw(%v) = %q err=%v, want %q", p.sonic, raw, err, want)
		}
	}
}

const scoreboardRecordCount = 1024

// scoreboardRecords builds an NDJSON-style corpus of 1024 flat records with
// sixteen scalar fields each, and returns the expected sum of the "ts" field
// for differential verification of the aggregation scenario.
func scoreboardRecords() (docs [][]byte, tsSum int64) {
	docs = make([][]byte, scoreboardRecordCount)
	for i := range docs {
		ts := int64(1_700_000_000_000) + int64(i)*997
		tsSum += ts
		var d []byte
		d = fmt.Appendf(d, `{"id":%d,`, i)
		d = fmt.Appendf(d, `"seq":%d,`, i*3)
		d = fmt.Appendf(d, `"ts":%d,`, ts)
		d = fmt.Appendf(d, `"score":%d.%02d,`, i%97, i%100)
		d = fmt.Appendf(d, `"ratio":0.%04d,`, i%9973)
		d = fmt.Appendf(d, `"active":%t,`, i%2 == 0)
		d = fmt.Appendf(d, `"deleted":%t,`, i%7 == 0)
		d = fmt.Appendf(d, `"region":"region-%02d",`, i%16)
		d = fmt.Appendf(d, `"name":"record-%04d-%c",`, i, 'a'+rune(i%26))
		d = fmt.Appendf(d, `"email":"user%d@example.com",`, i)
		d = fmt.Appendf(d, `"city":"city-%03d",`, i%512)
		d = fmt.Appendf(d, `"country":"country-%02d",`, i%32)
		d = fmt.Appendf(d, `"device":"device-%03d",`, i%128)
		d = fmt.Appendf(d, `"version":"1.%d.%d",`, i%10, i%25)
		d = fmt.Appendf(d, `"status":"%s",`, []string{"ok", "retry", "failed"}[i%3])
		d = fmt.Appendf(d, `"note":"synthetic record %d for the extraction scoreboard"}`, i)
		docs[i] = d
	}
	return docs, tsSum
}

// scoreboardDocSet indexes the record corpus once with hashed keys, the
// configuration compiled lookups and shape columns are designed for.
func scoreboardDocSet(tb testing.TB, docs [][]byte) *simdjson.DocSet {
	tb.Helper()
	var set simdjson.DocSet
	set.Options = document.IndexOptions{HashKeys: true}
	for _, doc := range docs {
		if _, err := set.Append(doc); err != nil {
			tb.Fatalf("DocSet.Append: %v", err)
		}
	}
	return &set
}

// TestLookupScoreboardAgreement pins the cross-library contract the
// benchmarks rely on: same paths, same raw tokens, same aggregate.
func TestLookupScoreboardAgreement(t *testing.T) {
	src := scoreboardTwitter(t)
	scoreboardCheckPaths(t, src)

	docs, tsSum := scoreboardRecords()
	set := scoreboardDocSet(t, docs)
	var cache simdjson.ShapeCache

	col := cache.AppendField(nil, set, "name")
	if len(col) != len(docs) {
		t.Fatalf("AppendField returned %d cells, want %d", len(col), len(docs))
	}
	for i, rv := range col {
		if want := gjson.GetBytes(docs[i], "name").Raw; string(rv.Bytes()) != want {
			t.Fatalf("doc %d: column %q, gjson %q", i, rv.Bytes(), want)
		}
	}

	cells, valid := cache.AppendFieldInt64(nil, nil, set, "ts")
	var sum int64
	for i, v := range cells {
		if !valid[i] {
			t.Fatalf("doc %d: ts column cell invalid", i)
		}
		sum += v
	}
	var gsum int64
	for _, doc := range docs {
		gsum += gjson.GetBytes(doc, "ts").Int()
	}
	if sum != tsSum || gsum != tsSum {
		t.Fatalf("ts sums diverge: column=%d gjson=%d want=%d", sum, gsum, tsSum)
	}
}

// BenchmarkPointLookupCold extracts a single nested path from one cold
// document: no state survives an iteration.
func BenchmarkPointLookupCold(b *testing.B) {
	src := scoreboardTwitter(b)
	want, ok, err := simdjson.GetRaw(src, scoreboardPointPointer)
	if err != nil || !ok {
		b.Fatalf("point path missing: ok=%v err=%v", ok, err)
	}
	wantRaw := string(want.Bytes())

	b.Run("gjson-GetBytes", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			r := gjson.GetBytes(src, scoreboardPointGJSON)
			if r.Raw != wantRaw {
				b.Fatalf("got %q", r.Raw)
			}
			lookupSinkInt += len(r.Str)
		}
	})
	b.Run("sonic-ast-Get", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			n, err := sonic.Get(src, scoreboardPointSonic...)
			if err != nil {
				b.Fatal(err)
			}
			s, err := n.String()
			if err != nil {
				b.Fatal(err)
			}
			lookupSinkInt += len(s)
		}
	})
	b.Run("stdlib-map", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var m map[string]any
			if err := stdjson.Unmarshal(src, &m); err != nil {
				b.Fatal(err)
			}
			user := m["statuses"].([]any)[50].(map[string]any)["user"].(map[string]any)
			lookupSinkInt += len(user["screen_name"].(string))
		}
	})
	b.Run("simdjson-ScanFirstRaw", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rv, ok, err := simdjson.ScanFirstRaw(src, scoreboardPointPointer)
			if err != nil || !ok {
				b.Fatalf("ok=%v err=%v", ok, err)
			}
			s, _, err := rv.Text()
			if err != nil {
				b.Fatal(err)
			}
			lookupSinkInt += len(s)
		}
	})
	b.Run("simdjson-ScanFirstRawTrusted", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rv, ok, err := simdjson.ScanFirstRawTrusted(src, scoreboardPointPointer)
			if err != nil || !ok {
				b.Fatalf("ok=%v err=%v", ok, err)
			}
			s, _, err := rv.Text()
			if err != nil {
				b.Fatal(err)
			}
			lookupSinkInt += len(s)
		}
	})
	b.Run("simdjson-GetRaw", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rv, ok, err := simdjson.GetRaw(src, scoreboardPointPointer)
			if err != nil || !ok {
				b.Fatalf("ok=%v err=%v", ok, err)
			}
			s, _, err := rv.Text()
			if err != nil {
				b.Fatal(err)
			}
			lookupSinkInt += len(s)
		}
	})
}

// reportPerLookup rescales an iteration that performs one lookup per
// scoreboard path into a per-lookup metric.
func reportPerLookup(b *testing.B) {
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(len(scoreboardPaths)), "ns/lookup")
}

// BenchmarkRepeatedLookup16 resolves the sixteen scoreboard paths against one
// document per iteration. gjson re-scans the raw bytes per path,
// sonic materializes a lazy tree once and navigates it, and simdjson builds a
// hashed index once and answers compiled pointers from it. The prebuilt
// variant isolates the marginal lookup cost once the index exists.
func BenchmarkRepeatedLookup16(b *testing.B) {
	src := scoreboardTwitter(b)
	scoreboardCheckPaths(b, src)

	gpaths := make([]string, len(scoreboardPaths))
	compiled := make([]simdjson.CompiledPointer, len(scoreboardPaths))
	for i, p := range scoreboardPaths {
		gpaths[i] = p.gjson
		compiled[i] = simdjson.MustCompilePointer(p.pointer)
	}

	b.Run("gjson-16xGetBytes", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			total := 0
			for _, p := range gpaths {
				total += len(gjson.GetBytes(src, p).Raw)
			}
			lookupSinkInt += total
		}
		reportPerLookup(b)
	})
	b.Run("gjson-GetManyBytes", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			total := 0
			for _, r := range gjson.GetManyBytes(src, gpaths...) {
				total += len(r.Raw)
			}
			lookupSinkInt += total
		}
		reportPerLookup(b)
	})
	b.Run("sonic-ast-parse-once", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			root, err := sonic.Get(src)
			if err != nil {
				b.Fatal(err)
			}
			total := 0
			for _, p := range scoreboardPaths {
				raw, err := root.GetByPath(p.sonic...).Raw()
				if err != nil {
					b.Fatal(err)
				}
				total += len(raw)
			}
			lookupSinkInt += total
		}
		reportPerLookup(b)
	})
	b.Run("simdjson-BuildIndex+16", func(b *testing.B) {
		need, err := simdjson.RequiredIndexEntries(src)
		if err != nil {
			b.Fatal(err)
		}
		storage := make([]simdjson.IndexEntry, need)
		opts := document.IndexOptions{HashKeys: true}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			idx, err := simdjson.BuildIndexOptions(src, storage, opts)
			if err != nil {
				b.Fatal(err)
			}
			total := 0
			for _, p := range compiled {
				n, ok, err := idx.PointerCompiled(p)
				if err != nil || !ok {
					b.Fatalf("ok=%v err=%v", ok, err)
				}
				total += len(n.Raw().Bytes())
			}
			lookupSinkInt += total
		}
		reportPerLookup(b)
	})
	b.Run("simdjson-prebuilt-16", func(b *testing.B) {
		need, err := simdjson.RequiredIndexEntries(src)
		if err != nil {
			b.Fatal(err)
		}
		idx, err := simdjson.BuildIndexOptions(src, make([]simdjson.IndexEntry, need), document.IndexOptions{HashKeys: true})
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			total := 0
			for _, p := range compiled {
				n, ok, err := idx.PointerCompiled(p)
				if err != nil || !ok {
					b.Fatalf("ok=%v err=%v", ok, err)
				}
				total += len(n.Raw().Bytes())
			}
			lookupSinkInt += total
		}
		reportPerLookup(b)
	})
}

// reportPerDoc rescales an iteration that visits every record once into a
// per-document metric.
func reportPerDoc(b *testing.B) {
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(scoreboardRecordCount), "ns/doc")
}

// BenchmarkCorpusFieldExtract pulls the raw "name" field from each of the
// 1024 records. The amortized simdjson variant reuses a DocSet built once
// outside the timer; the including-build variant pays for arena, index, and
// shape construction inside every iteration.
func BenchmarkCorpusFieldExtract(b *testing.B) {
	docs, _ := scoreboardRecords()
	const field = "name"

	b.Run("gjson-per-doc", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			total := 0
			for _, doc := range docs {
				total += len(gjson.GetBytes(doc, field).Raw)
			}
			lookupSinkInt += total
		}
		reportPerDoc(b)
	})
	b.Run("sonic-ast-per-doc", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			total := 0
			for _, doc := range docs {
				n, err := sonic.Get(doc, field)
				if err != nil {
					b.Fatal(err)
				}
				raw, err := n.Raw()
				if err != nil {
					b.Fatal(err)
				}
				total += len(raw)
			}
			lookupSinkInt += total
		}
		reportPerDoc(b)
	})
	b.Run("simdjson-docset-amortized", func(b *testing.B) {
		set := scoreboardDocSet(b, docs)
		var cache simdjson.ShapeCache
		col := make([]simdjson.RawValue, 0, set.Len())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			col = cache.AppendField(col[:0], set, field)
			total := 0
			for _, rv := range col {
				total += len(rv.Bytes())
			}
			lookupSinkInt += total
		}
		reportPerDoc(b)
	})
	b.Run("simdjson-docset-including-build", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var set simdjson.DocSet
			set.Options = document.IndexOptions{HashKeys: true}
			for _, doc := range docs {
				if _, err := set.Append(doc); err != nil {
					b.Fatal(err)
				}
			}
			var cache simdjson.ShapeCache
			col := cache.AppendField(nil, &set, field)
			total := 0
			for _, rv := range col {
				total += len(rv.Bytes())
			}
			lookupSinkInt += total
		}
		reportPerDoc(b)
	})
}

// BenchmarkCorpusSumInt64 sums the int64 "ts" field across the 1024 records
// and verifies the aggregate every iteration, so a wrong fast path cannot win.
func BenchmarkCorpusSumInt64(b *testing.B) {
	docs, tsSum := scoreboardRecords()
	const field = "ts"

	b.Run("gjson-per-doc", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var sum int64
			for _, doc := range docs {
				sum += gjson.GetBytes(doc, field).Int()
			}
			if sum != tsSum {
				b.Fatalf("sum = %d, want %d", sum, tsSum)
			}
			lookupSinkInt64 += sum
		}
		reportPerDoc(b)
	})
	b.Run("sonic-ast-per-doc", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var sum int64
			for _, doc := range docs {
				n, err := sonic.Get(doc, field)
				if err != nil {
					b.Fatal(err)
				}
				v, err := n.Int64()
				if err != nil {
					b.Fatal(err)
				}
				sum += v
			}
			if sum != tsSum {
				b.Fatalf("sum = %d, want %d", sum, tsSum)
			}
			lookupSinkInt64 += sum
		}
		reportPerDoc(b)
	})
	b.Run("stdlib-struct-per-doc", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var sum int64
			for _, doc := range docs {
				var rec struct {
					TS int64 `json:"ts"`
				}
				if err := stdjson.Unmarshal(doc, &rec); err != nil {
					b.Fatal(err)
				}
				sum += rec.TS
			}
			if sum != tsSum {
				b.Fatalf("sum = %d, want %d", sum, tsSum)
			}
			lookupSinkInt64 += sum
		}
		reportPerDoc(b)
	})
	b.Run("simdjson-column-amortized", func(b *testing.B) {
		set := scoreboardDocSet(b, docs)
		var cache simdjson.ShapeCache
		cells := make([]int64, 0, set.Len())
		valid := make([]bool, 0, set.Len())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			cells, valid = cache.AppendFieldInt64(cells[:0], valid[:0], set, field)
			var sum int64
			for _, v := range cells {
				sum += v
			}
			if sum != tsSum {
				b.Fatalf("sum = %d, want %d", sum, tsSum)
			}
			lookupSinkInt64 += sum
		}
		reportPerDoc(b)
	})
	b.Run("simdjson-column-including-build", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var set simdjson.DocSet
			set.Options = document.IndexOptions{HashKeys: true}
			for _, doc := range docs {
				if _, err := set.Append(doc); err != nil {
					b.Fatal(err)
				}
			}
			var cache simdjson.ShapeCache
			cells, _ := cache.AppendFieldInt64(nil, nil, &set, field)
			var sum int64
			for _, v := range cells {
				sum += v
			}
			if sum != tsSum {
				b.Fatalf("sum = %d, want %d", sum, tsSum)
			}
			lookupSinkInt64 += sum
		}
		reportPerDoc(b)
	})
}

// BenchmarkValidateAndIndex compares what each library charges before it will
// vouch for a document. The rows are not equivalent contracts: gjson.Valid
// and the stdlib validate without building anything, sonic's LoadAll
// materializes a full tree, and BuildIndex validates everything and leaves a
// queryable index behind.
func BenchmarkValidateAndIndex(b *testing.B) {
	src := scoreboardTwitter(b)

	b.Run("gjson-Valid", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !gjson.ValidBytes(src) {
				b.Fatal("invalid")
			}
		}
	})
	b.Run("sonic-Valid", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !sonic.Valid(src) {
				b.Fatal("invalid")
			}
		}
	})
	b.Run("sonic-ast-LoadAll", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			root, err := sonic.Get(src)
			if err != nil {
				b.Fatal(err)
			}
			if err := root.LoadAll(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("stdlib-Valid", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !stdjson.Valid(src) {
				b.Fatal("invalid")
			}
		}
	})
	b.Run("simdjson-Valid", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !simdjson.Valid(src) {
				b.Fatal("invalid")
			}
		}
	})
	b.Run("simdjson-BuildIndex", func(b *testing.B) {
		need, err := simdjson.RequiredIndexEntries(src)
		if err != nil {
			b.Fatal(err)
		}
		storage := make([]simdjson.IndexEntry, need)
		opts := document.IndexOptions{HashKeys: true}
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			idx, err := simdjson.BuildIndexOptions(src, storage, opts)
			if err != nil {
				b.Fatal(err)
			}
			lookupSinkInt += idx.Len()
		}
	})
}
