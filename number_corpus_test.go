package slopjson

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// This file builds number- and structure-heavy benchmark corpora that mirror
// the shapes the C++ simdjson suite is measured on, so the Go library can be
// profiled where it competes: long-mantissa float parsing (canada.json),
// dense flat number arrays (numbers.json), integer-and-structure documents
// (citm_catalog.json), and the structural indexing that underlies them all.
//
// The corpora are generated deterministically from a fixed seed rather than
// vendored as multi-megabyte blobs, so they cost nothing in the tree and can be
// scaled by record count. loadSimdjsonCorpus additionally lets the canonical
// files be dropped into testdata for a direct, byte-identical comparison with
// published C++ numbers.

// numberCorpusRand returns a deterministic source so every run frames the same
// bytes; the seed is arbitrary but fixed.
func numberCorpusRand() *rand.Rand {
	return rand.New(rand.NewSource(0x5eed1234))
}

// fmtLongFloat formats a value in [lo,hi) at its shortest round-trip spelling,
// the 15-to-17 significant digit shape real encoders emit (json.Marshal,
// JSON.stringify) and that canada.json carries. These miss the small-integer
// fast paths and, when not exactly representable, exercise Eisel-Lemire rather
// than being over-specified into the truncated slow path.
func fmtLongFloat(dst []byte, rng *rand.Rand, lo, hi float64) []byte {
	v := lo + rng.Float64()*(hi-lo)
	return strconv.AppendFloat(dst, v, 'g', -1, 64)
}

// coordRingsJSON mirrors a GeoJSON coordinate ring: a nested array of
// [longitude, latitude] pairs whose members are long-mantissa floats. It is the
// canada.json analogue and the sharpest float-parsing stress in the suite.
func coordRingsJSON(pairs int) []byte {
	rng := numberCorpusRand()
	dst := make([]byte, 0, pairs*40)
	dst = append(dst, `{"type":"Polygon","coordinates":[[`...)
	for i := 0; i < pairs; i++ {
		if i != 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '[')
		dst = fmtLongFloat(dst, rng, -180, 180)
		dst = append(dst, ',')
		dst = fmtLongFloat(dst, rng, -90, 90)
		dst = append(dst, ']')
	}
	dst = append(dst, `]]}`...)
	return dst
}

// floatArrayJSON is a dense flat array of mixed-magnitude floats: the
// numbers.json analogue, minimal structure and maximal number throughput. It
// interleaves plain-decimal and scientific spellings so both number sub-paths
// are exercised.
func floatArrayJSON(count int) []byte {
	rng := numberCorpusRand()
	dst := make([]byte, 0, count*20)
	dst = append(dst, '[')
	for i := 0; i < count; i++ {
		if i != 0 {
			dst = append(dst, ',')
		}
		switch i % 4 {
		case 0:
			dst = fmtLongFloat(dst, rng, -1e6, 1e6)
		case 1:
			dst = strconv.AppendFloat(dst, rng.NormFloat64()*1e9, 'e', -1, 64)
		case 2:
			dst = fmtLongFloat(dst, rng, 0, 1)
		default:
			dst = strconv.AppendFloat(dst, rng.NormFloat64(), 'g', -1, 64)
		}
	}
	dst = append(dst, ']')
	return dst
}

// sciFloatArrayJSON is a flat array of floats whose exponents span the whole
// double range, so most elements fall outside the [-22,37] exact-multiply
// envelope and exercise the Eisel-Lemire path (rather than the scalar strconv
// fallback). This is the shape scientific and engineering data carry.
func sciFloatArrayJSON(count int) []byte {
	rng := numberCorpusRand()
	dst := make([]byte, 0, count*24)
	dst = append(dst, '[')
	for i := 0; i < count; i++ {
		if i != 0 {
			dst = append(dst, ',')
		}
		// Shortest significand times a wide-range power of ten keeps the
		// mantissa realistic (<=17 digits) while pushing the exponent well
		// past the exact envelope in both directions.
		mant := rng.Float64()*9 + 1
		exp := rng.Intn(180) - 90
		dst = strconv.AppendFloat(dst, mant, 'g', -1, 64)
		dst = append(dst, 'e')
		dst = strconv.AppendInt(dst, int64(exp), 10)
	}
	dst = append(dst, ']')
	return dst
}

// intArrayJSON is a flat array of integers spanning the widths JSON documents
// actually carry: single digits, id-sized values, millisecond timestamps, and
// near-int64-max magnitudes that reach the 16-digit store/parse kernels.
func intArrayJSON(count int) []byte {
	rng := numberCorpusRand()
	dst := make([]byte, 0, count*14)
	dst = append(dst, '[')
	for i := 0; i < count; i++ {
		if i != 0 {
			dst = append(dst, ',')
		}
		var v int64
		switch i % 4 {
		case 0:
			v = int64(rng.Intn(10))
		case 1:
			v = int64(rng.Intn(1_000_000_000))
		case 2:
			v = 1_500_000_000_000 + int64(rng.Intn(100_000_000))
		default:
			v = rng.Int63()
		}
		if i&1 == 0 {
			v = -v
		}
		dst = strconv.AppendInt(dst, v, 10)
	}
	dst = append(dst, ']')
	return dst
}

// citmEvent is the integer-and-structure record decoded from citmLikeJSON.
type citmEvent struct {
	ID       int64   `json:"id"`
	Start    int64   `json:"start"`
	Price    float64 `json:"price"`
	Seats    int     `json:"seats"`
	Name     string  `json:"name"`
	SoldOut  bool    `json:"soldOut"`
	Sections []int   `json:"sections"`
}

type citmCatalog struct {
	Events []citmEvent `json:"events"`
}

// citmLikeJSON mirrors citm_catalog.json: many records dominated by integer IDs
// and timestamps with small structural objects around them, the shape that
// stresses integer parsing and object-field dispatch together.
func citmLikeJSON(events int) []byte {
	rng := numberCorpusRand()
	dst := make([]byte, 0, events*96)
	dst = append(dst, `{"events":[`...)
	for i := 0; i < events; i++ {
		if i != 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"id":`...)
		dst = strconv.AppendInt(dst, 100_000_000+int64(rng.Intn(900_000_000)), 10)
		dst = append(dst, `,"start":`...)
		dst = strconv.AppendInt(dst, 1_500_000_000_000+int64(rng.Intn(100_000_000)), 10)
		dst = append(dst, `,"price":`...)
		dst = strconv.AppendFloat(dst, float64(rng.Intn(50000))/100, 'f', 2, 64)
		dst = append(dst, `,"seats":`...)
		dst = strconv.AppendInt(dst, int64(rng.Intn(1000)), 10)
		dst = append(dst, `,"name":"event-`...)
		dst = strconv.AppendInt(dst, int64(i), 10)
		dst = append(dst, `","soldOut":`...)
		if i&1 == 0 {
			dst = append(dst, "true"...)
		} else {
			dst = append(dst, "false"...)
		}
		dst = append(dst, `,"sections":[`...)
		for s := 0; s < 4; s++ {
			if s != 0 {
				dst = append(dst, ',')
			}
			dst = strconv.AppendInt(dst, int64(rng.Intn(500)), 10)
		}
		dst = append(dst, `]}`...)
	}
	dst = append(dst, `]}`...)
	return dst
}

// loadSimdjsonCorpus reads a canonical benchmark file from
// testdata/corpora/simdjson if present, so canada.json, numbers.json, and the
// rest can be measured byte-for-byte against published C++ figures. It skips
// cleanly when the optional file is absent.
func loadSimdjsonCorpus(tb testing.TB, name string) []byte {
	tb.Helper()
	path := filepath.Join("testdata", "corpora", "slopjson", name)
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("optional corpus %s not present (drop the canonical file in to compare): %v", name, err)
	}
	return data
}

// TestNumberCorpusValid asserts every generated corpus is well-formed JSON and
// agrees with encoding/json, so the benchmarks below measure parsing rather
// than accidental malformation.
func TestNumberCorpusValid(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"coordRings", coordRingsJSON(64)},
		{"floatArray", floatArrayJSON(64)},
		{"sciFloatArray", sciFloatArrayJSON(64)},
		{"intArray", intArrayJSON(64)},
		{"citm", citmLikeJSON(16)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strictJSONValid(tc.data) {
				t.Fatalf("generated %s is not strict JSON: %.120q", tc.name, tc.data)
			}
			var ref any
			if err := json.Unmarshal(tc.data, &ref); err != nil {
				t.Fatalf("encoding/json rejects generated %s: %v", tc.name, err)
			}
			if _, err := Parse(tc.data); err != nil {
				t.Fatalf("Parse rejects generated %s: %v", tc.name, err)
			}
		})
	}
}

// --- Structural indexing throughput (Parse builds the tape, numbers lazy) ---

func BenchmarkNumberCorpusParse(b *testing.B) {
	corpora := []struct {
		name string
		data []byte
	}{
		{"CoordRings", coordRingsJSON(4096)},
		{"FloatArray", floatArrayJSON(8192)},
		{"IntArray", intArrayJSON(8192)},
		{"Citm", citmLikeJSON(1024)},
	}
	for _, c := range corpora {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.data)))
			b.ReportAllocs()
			for range b.N {
				if _, err := Parse(c.data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// --- Number-parsing throughput (typed decode forces every number) ---

func benchmarkTypedDecode[T any](b *testing.B, src []byte) {
	b.Helper()
	decoder, err := CompileDecoder[T](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	var dst T
	if err := decoder.Decode(src, &dst); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var out T
		if err := decoder.Decode(src, &out); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkStdlibDecode[T any](b *testing.B, src []byte) {
	b.Helper()
	var dst T
	if err := json.Unmarshal(src, &dst); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var out T
		if err := json.Unmarshal(src, &out); err != nil {
			b.Fatal(err)
		}
	}
}

type coordDoc struct {
	Coordinates [][][2]float64 `json:"coordinates"`
}

func BenchmarkNumberCorpusDecode(b *testing.B) {
	coord := coordRingsJSON(4096)
	floats := floatArrayJSON(8192)
	sci := sciFloatArrayJSON(8192)
	ints := intArrayJSON(8192)
	citm := citmLikeJSON(1024)

	b.Run("CoordRings", func(b *testing.B) { benchmarkTypedDecode[coordDoc](b, coord) })
	b.Run("CoordRings/Stdlib", func(b *testing.B) { benchmarkStdlibDecode[coordDoc](b, coord) })
	b.Run("FloatArray", func(b *testing.B) { benchmarkTypedDecode[[]float64](b, floats) })
	b.Run("FloatArray/Stdlib", func(b *testing.B) { benchmarkStdlibDecode[[]float64](b, floats) })
	b.Run("SciFloat", func(b *testing.B) { benchmarkTypedDecode[[]float64](b, sci) })
	b.Run("SciFloat/Stdlib", func(b *testing.B) { benchmarkStdlibDecode[[]float64](b, sci) })
	b.Run("IntArray", func(b *testing.B) { benchmarkTypedDecode[[]int64](b, ints) })
	b.Run("IntArray/Stdlib", func(b *testing.B) { benchmarkStdlibDecode[[]int64](b, ints) })
	b.Run("Citm", func(b *testing.B) { benchmarkTypedDecode[citmCatalog](b, citm) })
	b.Run("Citm/Stdlib", func(b *testing.B) { benchmarkStdlibDecode[citmCatalog](b, citm) })
}

// --- Optional canonical-file benchmarks for direct C++ comparison ---

func BenchmarkCanonicalParse(b *testing.B) {
	for _, name := range []string{"canada.json", "numbers.json", "citm_catalog.json", "twitter.json"} {
		b.Run(name, func(b *testing.B) {
			data := loadSimdjsonCorpus(b, name)
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for range b.N {
				if _, err := Parse(data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
