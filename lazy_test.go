package vibejson

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"testing"

	"github.com/thesyncim/vibejson/document"
)

func valueToAny(t *testing.T, v Value) any {
	t.Helper()
	node := v.Node()
	switch node.Kind() {
	case document.Null:
		return nil
	case document.Bool:
		b, ok := v.Bool()
		if !ok {
			t.Fatal("Bool() failed on Bool kind")
		}
		return b
	case document.Number:
		s, ok := v.NumberText()
		if !ok {
			t.Fatal("NumberText() failed on Number kind")
		}
		return json.Number(s)
	case document.String:
		s, ok := v.Text()
		if !ok {
			t.Fatal("Text() failed on String kind")
		}
		return s
	case document.Array:
		n, _ := node.ArrayLen()
		out := make([]any, 0, n)
		iter, ok := node.ArrayIter()
		if !ok {
			t.Fatal("ArrayIter() failed on Array kind")
		}
		for {
			el, ok := iter.Next()
			if !ok {
				break
			}
			out = append(out, valueToAny(t, v.with(el)))
		}
		return out
	case document.Object:
		out := map[string]any{}
		iter, ok := node.ObjectIter()
		if !ok {
			t.Fatal("ObjectIter() failed on Object kind")
		}
		for {
			key, val, ok := iter.Next()
			if !ok {
				break
			}
			ks, _ := key.StringBytes()
			var keyStr string
			if ks != nil {
				keyStr = string(ks)
			} else {
				b, _ := key.AppendText(nil)
				keyStr = string(b)
			}
			out[keyStr] = valueToAny(t, v.with(val))
		}
		return out
	default:
		t.Fatalf("unexpected kind %v", node.Kind())
		return nil
	}
}

func normalizeNumbers(t *testing.T, v any) any {
	t.Helper()
	switch x := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return string(x)
		}
		return f
	case float64:
		return x
	case []any:
		for i := range x {
			x[i] = normalizeNumbers(t, x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = normalizeNumbers(t, x[k])
		}
		return x
	default:
		return v
	}
}

var lazyCorpus = func() map[string][]byte {
	return map[string][]byte{
		"citm":       citmLikeJSON(64),
		"intArray":   intArrayJSON(128),
		"floatArray": floatArrayJSON(128),
		"coordRings": coordRingsJSON(64),
		"scalar":     []byte(`  42.5 `),
		"emptyObj":   []byte(`{}`),
		"emptyArr":   []byte(`[]`),
		"nested":     []byte(`{"a":{"b":[1,2,{"c":"xéy"}]},"d":null,"e":true,"f":false}`),
		"dupKeys":    []byte(`{"k":1,"k":2,"k":3}`),
		"escapes":    []byte(`{"a\tb":"line1\nline2☃","emoji":"😀"}`),
	}
}()

func TestLazyMatchesAnyAndStdlib(t *testing.T) {
	for name, src := range lazyCorpus {
		t.Run(name, func(t *testing.T) {
			v, err := Parse(src)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			cursorAny := normalizeNumbers(t, valueToAny(t, v))
			anyAny := normalizeNumbers(t, v.Any())
			if !reflect.DeepEqual(cursorAny, anyAny) {
				t.Fatalf("cursor != Any\ncursor: %#v\nany:    %#v", cursorAny, anyAny)
			}

			var stdAny any
			dec := json.NewDecoder(bytes.NewReader(src))
			dec.UseNumber()
			if err := dec.Decode(&stdAny); err != nil {
				t.Fatalf("encoding/json: %v", err)
			}
			stdAny = normalizeNumbers(t, stdAny)
			if !reflect.DeepEqual(cursorAny, stdAny) {
				t.Fatalf("cursor != encoding/json\ncursor: %#v\nstd:    %#v", cursorAny, stdAny)
			}
		})
	}
}

func TestLazyZeroCopyMatches(t *testing.T) {
	for name, src := range lazyCorpus {
		t.Run(name, func(t *testing.T) {
			buf := append([]byte(nil), src...)
			zc, err := ParseOptions(buf, Options{ZeroCopy: true})
			if err != nil {
				t.Fatalf("ParseOptions(ZeroCopy): %v", err)
			}
			owned, err := Parse(src)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !reflect.DeepEqual(
				normalizeNumbers(t, valueToAny(t, zc)),
				normalizeNumbers(t, owned.Any()),
			) {
				t.Fatalf("zero-copy != owned for %s", name)
			}
		})
	}
}

func TestLazyPartialGCSafe(t *testing.T) {
	makeAndRead := func() (int64, bool) {
		src := citmLikeJSON(64)
		v, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		id, ok, err := v.Pointer("/events/10/id")
		if err != nil || !ok {
			return 0, false
		}
		return id.Int64()
	}
	id, ok := makeAndRead()
	if !ok {
		t.Fatal("id missing")
	}
	runtime.GC()
	if id == 0 {
		t.Fatal("id read as zero after GC")
	}
}

func TestLazyDropSrcThenGC(t *testing.T) {
	src := []byte(`{"a":{"b":[1,2,{"c":"keep-me"}]}}`)
	v, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	for i := range src {
		src[i] = 0
	}
	src = nil
	_ = src
	runtime.GC()
	runtime.GC()
	got, ok, err := v.Pointer("/a/b/2/c")
	if err != nil || !ok {
		t.Fatalf("pointer: %v %v", ok, err)
	}
	text, ok := got.Text()
	if !ok || text != "keep-me" {
		t.Fatalf("after GC got %q ok=%v, want %q", text, ok, "keep-me")
	}
}

func TestLazyMarshalNormalizesEscapes(t *testing.T) {
	cases := []string{
		`"A"`,
		`"ABC"`,
		`"a\/b"`,
		`"é raw and é escaped"`,
		`"tab\tnew\nline"`,
		`"𝄞"`,
		`"\uFFFD escaped replacement"`,
		`"ctl \u0000 and \u007f done"`,
		"\"raw \u2028 and \u2029 separators\"",
		`"escaped \u2028 and \u2029 separators"`,
		`["A","\/","tab\ther",2,true,null]`,
	}
	for _, c := range cases {
		src := []byte(c)

		var v any
		if err := json.Unmarshal(src, &v); err != nil {
			t.Fatalf("stdlib rejected %s: %v", c, err)
		}
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("stdlib re-marshal %s: %v", c, err)
		}

		parsed, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse(%s): %v", c, err)
		}
		got, err := parsed.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON(%s): %v", c, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("MarshalJSON(%s) = %s, want %s (escape normalization diverged)", c, got, want)
		}
	}
}

func TestLazyFloatExactness(t *testing.T) {
	cases := []string{
		"0", "-0", "3.141592653589793", "1e308", "5e-324",
		"1.7976931348623157e308", "9007199254740993", "123456789012345678",
		"-0.0001", "2.2250738585072014e-308",
	}
	for _, c := range cases {
		src := []byte("[" + c + "]")
		v, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", c, err)
		}
		el, ok := v.Index(0)
		if !ok {
			t.Fatalf("%s: index 0 missing", c)
		}
		got, ok := el.Float64()
		if !ok {
			t.Fatalf("%s: Float64 failed", c)
		}
		want, _ := strconv.ParseFloat(c, 64)
		if got != want && !(math.IsNaN(got) && math.IsNaN(want)) {
			t.Fatalf("%s: got %v want %v", c, got, want)
		}
	}
}

func lazyBenchCorpus() []struct {
	name string
	data []byte
} {
	return []struct {
		name string
		data []byte
	}{
		{"Citm", citmLikeJSON(1024)},
		{"IntArray", intArrayJSON(8192)},
		{"FloatArray", floatArrayJSON(8192)},
		{"CoordRings", coordRingsJSON(4096)},
	}
}

func sumValueFull(v Value) float64 {
	node := v.Node()
	switch node.Kind() {
	case document.Number:
		f, _ := v.Float64()
		return f
	case document.Array:
		iter, _ := node.ArrayIter()
		var s float64
		for {
			el, ok := iter.Next()
			if !ok {
				break
			}
			s += sumValueFull(v.with(el))
		}
		return s
	case document.Object:
		iter, _ := node.ObjectIter()
		var s float64
		for {
			_, val, ok := iter.Next()
			if !ok {
				break
			}
			s += sumValueFull(v.with(val))
		}
		return s
	default:
		return 0
	}
}

var lazyFloatSink float64

func BenchmarkParseFull(b *testing.B) {
	for _, c := range lazyBenchCorpus() {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.data)))
			b.ReportAllocs()
			var s float64
			for range b.N {
				v, err := Parse(c.data)
				if err != nil {
					b.Fatal(err)
				}
				s += sumValueFull(v)
			}
			lazyFloatSink = s
		})
	}
}

func BenchmarkParsePartial(b *testing.B) {
	citm := citmLikeJSON(1024)
	ints := intArrayJSON(8192)
	b.Run("Citm", func(b *testing.B) {
		b.SetBytes(int64(len(citm)))
		b.ReportAllocs()
		var s float64
		for range b.N {
			v, err := Parse(citm)
			if err != nil {
				b.Fatal(err)
			}
			for _, idx := range []int{3, 100, 900} {
				ev, ok, _ := v.Pointer("/events/" + itoa(idx))
				if !ok {
					b.Fatal("event missing")
				}
				id, _ := ev.Get("id")
				price, _ := ev.Get("price")
				name, _ := ev.Get("name")
				fid, _ := id.Float64()
				fpr, _ := price.Float64()
				ntxt, _ := name.Text()
				s += fid + fpr + float64(len(ntxt))
			}
		}
		lazyFloatSink = s
	})
	b.Run("IntArray", func(b *testing.B) {
		b.SetBytes(int64(len(ints)))
		b.ReportAllocs()
		var s float64
		for range b.N {
			v, err := Parse(ints)
			if err != nil {
				b.Fatal(err)
			}
			for _, idx := range []int{7, 4000, 8000} {
				el, ok := v.Index(idx)
				if !ok {
					b.Fatal("index missing")
				}
				f, _ := el.Float64()
				s += f
			}
		}
		lazyFloatSink = s
	})
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestLazyPoolReuseIsolation(t *testing.T) {
	docs := [][]byte{
		[]byte(`{"n":1}`),
		[]byte(`[1,2,3,4,5]`),
		[]byte(`{"a":{"b":{"c":42}}}`),
		citmLikeJSON(8),
		[]byte(`"hello"`),
	}
	goroutines := testIterations(16, 8)
	iterations := testIterations(2_000, 100)
	done := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		go func(seed int) {
			for iter := 0; iter < iterations; iter++ {
				src := docs[(seed+iter)%len(docs)]
				v, err := Parse(src)
				if err != nil {
					done <- err
					return
				}
				want, _ := Parse(src)
				if !reflect.DeepEqual(
					normalizeNumbers(t, valueToAny(t, v)),
					normalizeNumbers(t, want.Any()),
				) {
					done <- errMismatch
					return
				}
			}
			done <- nil
		}(g)
	}
	for g := 0; g < goroutines; g++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

var errMismatch = &pointerErrorStub{"parsed Value content diverged under concurrent pool reuse"}

type pointerErrorStub struct{ msg string }

func (e *pointerErrorStub) Error() string { return e.msg }
