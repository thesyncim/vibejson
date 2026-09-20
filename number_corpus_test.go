package vibejson

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func numberCorpusRand() *rand.Rand {
	return rand.New(rand.NewSource(0x5eed1234))
}

func fmtLongFloat(dst []byte, rng *rand.Rand, lo, hi float64) []byte {
	v := lo + rng.Float64()*(hi-lo)
	return strconv.AppendFloat(dst, v, 'g', -1, 64)
}

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

func sciFloatArrayJSON(count int) []byte {
	rng := numberCorpusRand()
	dst := make([]byte, 0, count*24)
	dst = append(dst, '[')
	for i := 0; i < count; i++ {
		if i != 0 {
			dst = append(dst, ',')
		}
		mant := rng.Float64()*9 + 1
		exp := rng.Intn(180) - 90
		dst = strconv.AppendFloat(dst, mant, 'g', -1, 64)
		dst = append(dst, 'e')
		dst = strconv.AppendInt(dst, int64(exp), 10)
	}
	dst = append(dst, ']')
	return dst
}

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

func loadSimdjsonCorpus(tb testing.TB, name string) []byte {
	tb.Helper()
	path := filepath.Join("testdata", "corpora", "vibejson", name)
	data, err := os.ReadFile(path)
	if err != nil {
		tb.Skipf("optional corpus %s not present (drop the canonical file in to compare): %v", name, err)
	}
	return data
}

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
