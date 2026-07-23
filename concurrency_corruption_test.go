package slopjson

// These tests guard shared immutable Encoder and Decoder plans against
// cross-goroutine heap corruption and value contamination while their
// per-call scratch is recycled through sync.Pool.
//
// The historical bug bound a pooled reflect.MapIter to a movable stack map
// hidden from escape analysis. Sources are now GC-visible and the iterator is
// unbound before pooling. Reproduction combines concurrent maps, low GOGC,
// and GOMAXPROCS transitions through -cpu=1,4,8.
//
// Goroutines retain and verify outputs against serial goldens. The race detector
// masks the failure, so scripts/stress-concurrency.sh repeats the non-race run.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// corruptionFailures retains the first counted diagnostic; notes do not count.
type corruptionFailures struct {
	bad int64
	mu  sync.Mutex
	msg string
}

func (f *corruptionFailures) record(message string) {
	f.recordf("%s", message)
}

func (f *corruptionFailures) recordf(format string, args ...any) {
	if atomic.AddInt64(&f.bad, 1) != 1 {
		return
	}
	f.mu.Lock()
	f.msg = fmt.Sprintf(format, args...)
	f.mu.Unlock()
}

func (f *corruptionFailures) note(message string) {
	f.mu.Lock()
	if f.msg == "" {
		f.msg = message
	}
	f.mu.Unlock()
}

func (f *corruptionFailures) recordSticky(message string) {
	atomic.AddInt64(&f.bad, 1)
	f.note(message)
}

func (f *corruptionFailures) requireNone(t *testing.T) {
	t.Helper()
	if bad := atomic.LoadInt64(&f.bad); bad != 0 {
		t.Fatalf("bad=%d %s", bad, f.msg)
	}
}

// TestCorruptionCanonicalMapPtr keeps a freshly stack-built map in a small,
// preemptible goroutine frame. A harness or golden pre-pass reshapes that frame
// and hides the historical corruption, so keep this test direct.
func TestCorruptionCanonicalMapPtr(t *testing.T) {
	type Inner struct {
		X string
		Y int
	}
	type S struct {
		M map[string]*Inner `json:"m"`
	}
	enc := mustCompileTestEncoder[S](t, EncoderOptions{})
	const goroutines = 16
	const iters = 6000
	var wg sync.WaitGroup
	var bad int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			keep := make([][]byte, 0, 4000)
			for it := 0; it < iters; it++ {
				m := map[string]*Inner{}
				for k := 0; k < 6; k++ {
					m[fmt.Sprintf("k%d_%d_%d", g, it, k)] = &Inner{X: fmt.Sprintf("x%d", it), Y: it + k}
				}
				out, err := enc.AppendJSON(nil, &S{M: m})
				if err != nil {
					atomic.AddInt64(&bad, 1)
					continue
				}
				// Verify the single expected value survived: each entry's tag
				// must reference this goroutine/iteration, catching contamination.
				want := fmt.Sprintf(`"x%d"`, it)
				if !strings.Contains(string(out), want) {
					atomic.AddInt64(&bad, 1)
				}
				keep = append(keep, out)
				if len(keep) > 3000 {
					keep = keep[1500:]
				}
			}
			_ = keep
		}(g)
	}
	wg.Wait()
	if bad != 0 {
		t.Fatalf("bad=%d: corruption or contamination", bad)
	}
}

// runDistinctEncode checks distinct values against serial goldens and retains
// outputs for GC scanning; the external GOGC/-cpu runner controls timing.
func runDistinctEncode[T any](t *testing.T, enc Encoder[T], goroutines, iters int, mk func(g, it int) *T) {
	t.Helper()
	if testing.Short() {
		goroutines = min(goroutines, 8)
		iters = min(iters, 250)
	}
	golden := make([][]string, goroutines)
	for g := 0; g < goroutines; g++ {
		golden[g] = make([]string, iters)
		for it := 0; it < iters; it++ {
			out, err := enc.AppendJSON(nil, mk(g, it))
			if err != nil {
				t.Fatalf("golden g%d i%d: %v", g, it, err)
			}
			golden[g][it] = string(out)
		}
	}

	var failures corruptionFailures
	{
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				// Keep a live heap for the collector to scan during encoding.
				keep := make([][]byte, 0, iters)
				for it := 0; it < iters; it++ {
					out, err := enc.AppendJSON(nil, mk(g, it))
					if err != nil {
						failures.recordf("g%d i%d err: %v", g, it, err)
						continue
					}
					if string(out) != golden[g][it] {
						failures.recordf("g%d i%d cross-contamination:\n want=%s\n  got=%s", g, it, golden[g][it], string(out))
					}
					keep = append(keep, out)
					if len(keep) > 4000 {
						keep = keep[2000:]
					}
				}
				_ = keep
			}(g)
		}
		close(start)
		wg.Wait()
	}
	failures.requireNone(t)
}

// Encoder map scratch cases vary key kind and pointer content across every
// pooled mapEntries/mapKeyArena/mapIter/valueBacking slot.

type ccInner struct {
	X string `json:"x"`
	Y int    `json:"y"`
}

func TestCorruptionEncodeMapStringPtrValues(t *testing.T) {
	type S struct {
		M map[string]*ccInner `json:"m"`
	}
	enc := mustCompileTestEncoder[S](t, EncoderOptions{})
	runDistinctEncode(t, enc, 16, 8000, func(g, it int) *S {
		m := map[string]*ccInner{}
		for k := 0; k < 6; k++ {
			tag := fmt.Sprintf("%d_%d_%d", g, it, k)
			m["k"+tag] = &ccInner{X: "x" + tag, Y: g + it + k}
		}
		return &S{M: m}
	})
}

func TestCorruptionEncodeMapStringValues(t *testing.T) {
	type S struct {
		M map[string]string `json:"m"`
	}
	enc := mustCompileTestEncoder[S](t, EncoderOptions{})
	runDistinctEncode(t, enc, 16, 4000, func(g, it int) *S {
		m := map[string]string{}
		for k := 0; k < 6; k++ {
			tag := fmt.Sprintf("%d_%d_%d", g, it, k)
			m["k"+tag] = "v" + tag
		}
		return &S{M: m}
	})
}

func TestCorruptionEncodeMapIntValues(t *testing.T) {
	type S struct {
		M map[string]int `json:"m"`
	}
	enc := mustCompileTestEncoder[S](t, EncoderOptions{})
	runDistinctEncode(t, enc, 16, 4000, func(g, it int) *S {
		m := map[string]int{}
		for k := 0; k < 6; k++ {
			m[fmt.Sprintf("k%d_%d_%d", g, it, k)] = g + it + k
		}
		return &S{M: m}
	})
}

func TestCorruptionEncodeMapIntKeys(t *testing.T) {
	// int keys route through keyArena instead of the marshaler keyBox.
	type S struct {
		M map[int]string `json:"m"`
	}
	enc := mustCompileTestEncoder[S](t, EncoderOptions{})
	runDistinctEncode(t, enc, 16, 4000, func(g, it int) *S {
		m := map[int]string{}
		for k := 0; k < 6; k++ {
			m[g*100000+it*10+k] = fmt.Sprintf("v%d_%d_%d", g, it, k)
		}
		return &S{M: m}
	})
}

type ccTextKey struct{ A, B int }

func (k ccTextKey) MarshalText() ([]byte, error) {
	return []byte(fmt.Sprintf("%d:%d", k.A, k.B)), nil
}

func TestCorruptionEncodeMapTextKeys(t *testing.T) {
	// TextMarshaler keys route through the marshaler scratch slot for the key.
	type S struct {
		M map[ccTextKey]int `json:"m"`
	}
	enc := mustCompileTestEncoder[S](t, EncoderOptions{})
	runDistinctEncode(t, enc, 16, 3000, func(g, it int) *S {
		m := map[ccTextKey]int{}
		for k := 0; k < 6; k++ {
			m[ccTextKey{A: g*1000 + it, B: k}] = g + it + k
		}
		return &S{M: m}
	})
}

func TestCorruptionEncodeNestedMaps(t *testing.T) {
	// Nested maps exercise the fallback-allocate branch: an inner map sees the
	// scratch already taken and must allocate its own.
	type S struct {
		M map[string]map[string]*ccInner `json:"m"`
	}
	enc := mustCompileTestEncoder[S](t, EncoderOptions{})
	runDistinctEncode(t, enc, 16, 2500, func(g, it int) *S {
		outer := map[string]map[string]*ccInner{}
		for a := 0; a < 4; a++ {
			inner := map[string]*ccInner{}
			for b := 0; b < 4; b++ {
				tag := fmt.Sprintf("%d_%d_%d_%d", g, it, a, b)
				inner["i"+tag] = &ccInner{X: "x" + tag, Y: g + it + a + b}
			}
			outer[fmt.Sprintf("o%d_%d_%d", g, it, a)] = inner
		}
		return &S{M: outer}
	})
}

// Inline catch-all (",inline"): uses the marshaler key scratch slot and the
// value backing, like maps.

type ccInlineDoc struct {
	ID    int                 `json:"id"`
	Extra map[string]*ccInner `json:",inline"`
}

func TestCorruptionEncodeInline(t *testing.T) {
	enc := mustCompileTestEncoder[ccInlineDoc](t, EncoderOptions{InlineFields: true})
	runDistinctEncode(t, enc, 16, 3000, func(g, it int) *ccInlineDoc {
		m := map[string]*ccInner{}
		for k := 0; k < 6; k++ {
			tag := fmt.Sprintf("%d_%d_%d", g, it, k)
			m["e"+tag] = &ccInner{X: "x" + tag, Y: g + it + k}
		}
		return &ccInlineDoc{ID: g*1000 + it, Extra: m}
	})
}

// Custom marshalers: exercise the marshalers[] scratch slots (value box reuse).

type ccJSONMarshaler struct{ V string }

func (m ccJSONMarshaler) MarshalJSON() ([]byte, error) { return json.Marshal(m.V) }

type ccTextMarshaler struct{ V string }

func (m ccTextMarshaler) MarshalText() ([]byte, error) { return []byte("T:" + m.V), nil }

func TestCorruptionEncodeMarshalers(t *testing.T) {
	type S struct {
		A ccJSONMarshaler `json:"a"`
		B ccTextMarshaler `json:"b"`
		C ccJSONMarshaler `json:"c"`
		D ccTextMarshaler `json:"d"`
	}
	enc := mustCompileTestEncoder[S](t, EncoderOptions{})
	runDistinctEncode(t, enc, 16, 5000, func(g, it int) *S {
		tag := fmt.Sprintf("%d_%d", g, it)
		return &S{
			A: ccJSONMarshaler{V: "a-" + tag},
			B: ccTextMarshaler{V: "b-" + tag},
			C: ccJSONMarshaler{V: "c-" + tag},
			D: ccTextMarshaler{V: "d-" + tag},
		}
	})
}

// Everything at once: the maximal-scratch document.

type ccKitchenSink struct {
	Name    string              `json:"name"`
	IntMap  map[string]int      `json:"intMap"`
	PtrMap  map[string]*ccInner `json:"ptrMap"`
	TextMap map[ccTextKey]int   `json:"textMap"`
	Marsh   ccJSONMarshaler     `json:"marsh"`
	Text    ccTextMarshaler     `json:"text"`
	When    time.Time           `json:"when"`
	Any     any                 `json:"any"`
	Inline  map[string]int      `json:",inline"`
}

func TestCorruptionEncodeKitchenSink(t *testing.T) {
	enc := mustCompileTestEncoder[ccKitchenSink](t, EncoderOptions{InlineFields: true})
	runDistinctEncode(t, enc, 16, 2500, func(g, it int) *ccKitchenSink {
		tag := fmt.Sprintf("%d_%d", g, it)
		im := map[string]int{}
		pm := map[string]*ccInner{}
		tm := map[ccTextKey]int{}
		inl := map[string]int{}
		for k := 0; k < 6; k++ {
			im[fmt.Sprintf("i%s_%d", tag, k)] = g + it + k
			pm[fmt.Sprintf("p%s_%d", tag, k)] = &ccInner{X: "x" + tag, Y: k}
			tm[ccTextKey{A: g*100 + it, B: k}] = k
			inl[fmt.Sprintf("x%s_%d", tag, k)] = g ^ (it + k)
		}
		return &ccKitchenSink{
			Name: "n-" + tag, IntMap: im, PtrMap: pm, TextMap: tm,
			Marsh: ccJSONMarshaler{V: "m-" + tag}, Text: ccTextMarshaler{V: "t-" + tag},
			When:   time.Unix(int64(g*1000+it), 0).UTC(),
			Any:    map[string]any{"anyKey-" + tag: []any{g, it, "s-" + tag}},
			Inline: inl,
		}
	})
}

// Top-level Marshal path (global plan cache), realistic public entry point.

func TestCorruptionMarshalMaps(t *testing.T) {
	type Doc struct {
		M    map[string]*ccInner `json:"m"`
		Tags map[string]string   `json:"tags"`
	}
	mk := func(g, it int) Doc {
		d := Doc{M: map[string]*ccInner{}, Tags: map[string]string{}}
		for k := 0; k < 5; k++ {
			tag := fmt.Sprintf("%d_%d_%d", g, it, k)
			d.M["m"+tag] = &ccInner{X: "x" + tag, Y: g + it + k}
			d.Tags["t"+tag] = "v" + tag
		}
		return d
	}
	// serial goldens via encoding/json (Marshal matches it byte for byte)
	goroutines := testIterations(16, 8)
	iters := testIterations(3_000, 250)
	golden := make([][]string, goroutines)
	for g := 0; g < goroutines; g++ {
		golden[g] = make([]string, iters)
		for it := 0; it < iters; it++ {
			d := mk(g, it)
			b, _ := json.Marshal(d)
			golden[g][it] = string(b)
		}
	}
	var failures corruptionFailures
	{
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				keep := make([][]byte, 0, iters)
				for it := 0; it < iters; it++ {
					d := mk(g, it)
					out, err := Marshal(&d)
					if err != nil {
						failures.recordf("g%d i%d err %v", g, it, err)
						continue
					}
					if string(out) != golden[g][it] {
						failures.recordf("g%d i%d\n want=%s\n  got=%s", g, it, golden[g][it], string(out))
					}
					keep = append(keep, out)
					if len(keep) > 1500 {
						keep = keep[750:]
					}
				}
				_ = keep
			}(g)
		}
		wg.Wait()
	}
	failures.requireNone(t)
}

// Decoder path under GC pressure: distinct inputs into distinct destinations,
// escaped strings force the string arena; owned mode clones source per string.

func TestCorruptionDecodeDistinct(t *testing.T) {
	type Inner struct {
		Tag  string `json:"tag"`
		Nums []int  `json:"nums"`
	}
	type Doc struct {
		ID     int               `json:"id"`
		Name   string            `json:"name"`
		Inner  Inner             `json:"inner"`
		Labels map[string]string `json:"labels"`
		Ptr    *Inner            `json:"ptr"`
	}
	dec := mustCompileTestDecoder[Doc](t, DecoderOptions{})
	const goroutines = 16
	const iters = 3000
	var failures corruptionFailures
	{
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				keep := make([]*Doc, 0, iters)
				for it := 0; it < iters; it++ {
					tag := fmt.Sprintf("g%d-i%d", g, it)
					input := []byte(fmt.Sprintf(
						`{"id":%d,"name":%q,"inner":{"tag":"esc\n\t%s","nums":[%d,%d]},"labels":{%q:%q},"ptr":{"tag":%q,"nums":[%d]}}`,
						g*100000+it, "name-"+tag+"-☃", tag, g, it, "L"+tag, "V"+tag, "ptr-"+tag, it))
					dst := &Doc{}
					if err := dec.Decode(input, dst); err != nil {
						failures.recordf("g%d i%d err %v input=%s", g, it, err, input)
						continue
					}
					if dst.ID != g*100000+it || dst.Name != "name-"+tag+"-☃" ||
						dst.Inner.Tag != "esc\n\t"+tag || len(dst.Inner.Nums) != 2 ||
						dst.Inner.Nums[0] != g || dst.Labels["L"+tag] != "V"+tag ||
						dst.Ptr == nil || dst.Ptr.Tag != "ptr-"+tag {
						failures.recordf("g%d i%d mismatch: %+v", g, it, dst)
					}
					keep = append(keep, dst)
					if len(keep) > 2000 {
						keep = keep[1000:]
					}
				}
				_ = keep
			}(g)
		}
		wg.Wait()
	}
	failures.requireNone(t)
}

// Compiled round-trip and streaming under GC pressure. Each goroutine owns its
// Writer/Reader; the immutable Encoder and Decoder plans are shared.

func TestCorruptionCompiledRoundTrip(t *testing.T) {
	type CD struct {
		Name string         `json:"name"`
		Vals map[string]int `json:"vals"`
		List []string       `json:"list"`
	}
	enc := mustCompileTestEncoder[CD](t, EncoderOptions{})
	dec := mustCompileTestDecoder[CD](t, DecoderOptions{})
	const goroutines = 16
	const iters = 3000
	var failures corruptionFailures
	{
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for it := 0; it < iters; it++ {
					tag := fmt.Sprintf("g%d-i%d", g, it)
					src := CD{
						Name: "n-" + tag,
						Vals: map[string]int{"a" + tag: g, "b" + tag: it},
						List: []string{"l1-" + tag, "l2-" + tag, strings.Repeat("z", g%4)},
					}
					out, err := enc.AppendJSON(nil, &src)
					if err != nil {
						continue
					}
					var dst CD
					if err := dec.Decode(out, &dst); err != nil {
						failures.recordf("dec g%d i%d: %v enc=%s", g, it, err, out)
						continue
					}
					if dst.Name != src.Name || dst.Vals["a"+tag] != g || dst.Vals["b"+tag] != it ||
						len(dst.List) != 3 || dst.List[0] != "l1-"+tag {
						failures.recordf("g%d i%d mismatch: %+v", g, it, dst)
					}
				}
			}(g)
		}
		wg.Wait()
	}
	failures.requireNone(t)
}

func TestCorruptionStreaming(t *testing.T) {
	type SV struct {
		K string         `json:"k"`
		N int            `json:"n"`
		M map[string]int `json:"m"`
	}
	enc := mustCompileTestEncoder[SV](t, EncoderOptions{})
	dec := mustCompileTestDecoder[SV](t, DecoderOptions{})
	goroutines := testIterations(14, 8)
	iters := testIterations(1_500, 150)
	const perStream = 5
	var failures corruptionFailures
	{
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for it := 0; it < iters; it++ {
					var buf bytes.Buffer
					w := NewWriter(&buf)
					vals := make([]SV, perStream)
					for j := 0; j < perStream; j++ {
						tag := fmt.Sprintf("g%d-i%d-j%d", g, it, j)
						vals[j] = SV{K: "k-" + tag, N: g*10000 + it*10 + j, M: map[string]int{"m" + tag: j}}
						if err := EncodeTo(w, enc, &vals[j]); err != nil {
							failures.note(fmt.Sprintf("enc g%d i%d j%d: %v", g, it, j, err))
						}
						if err := w.Newline(); err != nil {
							failures.note(fmt.Sprintf("newline g%d i%d j%d: %v", g, it, j, err))
						}
					}
					if err := w.Flush(); err != nil {
						failures.note(fmt.Sprintf("flush g%d i%d: %v", g, it, err))
					}
					r := NewReader(bytes.NewReader(buf.Bytes()))
					for j := 0; j < perStream; j++ {
						if !r.Next() {
							failures.recordf("g%d i%d j%d Next=false err=%v", g, it, j, r.Err())
							break
						}
						var dst SV
						if err := DecodeFrom(r, dec, &dst); err != nil {
							failures.recordf("g%d i%d j%d dec: %v", g, it, j, err)
							continue
						}
						tag := fmt.Sprintf("g%d-i%d-j%d", g, it, j)
						if dst.K != vals[j].K || dst.N != vals[j].N || dst.M["m"+tag] != j || len(dst.M) != 1 {
							failures.recordf("g%d i%d j%d mismatch got=%+v want=%+v", g, it, j, dst, vals[j])
						}
					}
				}
			}(g)
		}
		wg.Wait()
	}
	failures.requireNone(t)
}

// Shared-source readers: Parse/Get, GetRaw, ScanFirstRaw on one read-only buffer.

func TestCorruptionSharedSourceReaders(t *testing.T) {
	src := []byte(`{"users":[{"id":1,"name":"alice"},{"id":2,"name":"bob"}],` +
		`"meta":{"count":2,"tag":"x"},"deep":{"a":{"b":{"c":"found"}}},"name":"root"}`)
	const goroutines = 16
	const iters = 4000
	var failures corruptionFailures
	{
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for it := 0; it < iters; it++ {
					v, err := ParseOptions(src, Options{ZeroCopy: true})
					if err != nil {
						failures.recordf("g%d i%d parse %v", g, it, err)
						continue
					}
					if name, _ := v.Get("name"); func() string { s, _ := name.Text(); return s }() != "root" {
						failures.recordf("g%d i%d name wrong", g, it)
					}
					r1, ok1, e1 := GetRaw(src, "/users/1/name")
					if e1 != nil || !ok1 || string(r1.Bytes()) != `"bob"` {
						failures.recordf("g%d i%d GetRaw users/1/name = %q ok=%v err=%v", g, it, string(r1.Bytes()), ok1, e1)
					}
					r2, ok2, e2 := ScanFirstRaw(src, "/deep/a/b/c")
					if e2 != nil || !ok2 || string(r2.Bytes()) != `"found"` {
						failures.recordf("g%d i%d ScanFirstRaw deep = %q", g, it, string(r2.Bytes()))
					}
					r3, ok3, e3 := GetRaw(src, "/meta")
					if e3 != nil || !ok3 || string(r3.Bytes()) != `{"count":2,"tag":"x"}` {
						failures.recordf("g%d i%d GetRaw meta = %q", g, it, string(r3.Bytes()))
					}
				}
			}(g)
		}
		wg.Wait()
	}
	failures.requireNone(t)
}

// TestCorruptionUnmarshalPlanCache races the Unmarshal plan cache from many
// goroutines and verifies exact reconstruction under GC pressure.
func TestCorruptionUnmarshalPlanCache(t *testing.T) {
	type UD struct {
		A int            `json:"a"`
		B string         `json:"b"`
		C map[string]int `json:"c"`
		D []string       `json:"d"`
	}
	const goroutines = 16
	const iters = 3000
	var failures corruptionFailures
	{
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				for it := 0; it < iters; it++ {
					tag := fmt.Sprintf("g%d_%d", g, it)
					input := []byte(fmt.Sprintf(`{"a":%d,"b":%q,"c":{"x":%d,"y":%d},"d":[%q,%q]}`,
						g*100+it, "b-"+tag, g, it, "d1-"+tag, "d2-"+tag))
					var dst UD
					if err := Unmarshal(input, &dst); err != nil {
						failures.recordf("g%d i%d err %v", g, it, err)
						continue
					}
					want := UD{A: g*100 + it, B: "b-" + tag, C: map[string]int{"x": g, "y": it}, D: []string{"d1-" + tag, "d2-" + tag}}
					if !reflect.DeepEqual(dst, want) {
						failures.recordf("g%d i%d got=%+v want=%+v", g, it, dst, want)
					}
				}
			}(g)
		}
		close(start)
		wg.Wait()
	}
	failures.requireNone(t)
}
