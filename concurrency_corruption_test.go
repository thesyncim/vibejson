package simdjson

// Concurrency corruption regression suite.
//
// These tests guard against cross-goroutine heap corruption in the shared,
// immutable Encoder/Decoder/Codec objects whose per-call state is recycled
// through sync.Pool scratch. A regression here manifests as "found bad pointer
// in Go heap" / "found pointer to free object" fatal GC errors, or as one
// goroutine observing another's data.
//
// The historical bug: encodeMap bound a reused (pooled, heap-resident)
// reflect.MapIter to a map value whose internal pointer had been hidden from
// escape analysis and still aliased the goroutine stack. A stack move could
// leave the iterator's copy dangling. Sources are now ordinarily GC-visible,
// and releaseMapScratch unbinds the iterator before it enters the pool.
//
// Detecting this class reliably needs three ingredients TOGETHER, supplied by
// the test runner, not by the test body (in-process GC/GOMAXPROCS twiddling
// actually suppresses it):
//
//  1. concurrency: many goroutines encoding distinct maps at once;
//  2. GC pressure: a low GOGC so the collector runs during encoding;
//  3. stack moves: GOMAXPROCS transitions mid-run via -cpu=1,4,8.
//
// Each goroutine retains its outputs (so the collector scans a large live heap)
// and verifies every result byte for byte against a serial golden, catching
// both fatal corruption and silent value cross-contamination. Run the suite
// plainly for a smoke check, and under the stress invocation to actually
// exercise the corruption window. A masking effect makes -race far LESS likely
// to catch it, so the stress run is deliberately without -race:
//
//	# smoke (fast, always in CI):
//	GOEXPERIMENT=simd gotip test -run TestCorruption -count=5 ./
//	GOEXPERIMENT=simd gotip test -race -run TestCorruption -count=3 ./
//
//	# stress (the invocation that reproduced the historical bug ~8/8):
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestCorruption -count=5 -cpu=1,4,8 ./
//
// scripts/stress-concurrency.sh runs the stress invocation in a loop.

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

// TestCorruptionCanonicalMapPtr is the primary, deterministic detector for the
// historical stack-move corruption. It is written directly (no generic helper
// or golden pre-pass) because the bug depends on the encode call sitting in a
// small, preemptible goroutine frame with the map value freshly stack-built:
// routing through a generic harness reshapes the frame and hides it. Under the
// stress invocation (GOGC=1 -cpu=1,4,8) this crashed the unfixed encoder on the
// first round; the GC-visible source and explicit iterator unbind make it
// clean. Keep this shape.
func TestCorruptionCanonicalMapPtr(t *testing.T) {
	type Inner struct {
		X string
		Y int
	}
	type S struct {
		M map[string]*Inner `json:"m"`
	}
	enc, err := CompileEncoder[S](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
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

// runDistinctEncode is the coverage harness. mk builds the g/iter-distinct
// value; the harness computes serial goldens, then encodes them concurrently —
// plain, so the external runner (GOGC + -cpu) governs GC and stack-move timing —
// retaining outputs so the collector scans a large live heap, and asserts each
// result equals its golden. It verifies value integrity across every scratch
// path; TestCorruptionCanonicalMapPtr is the reliable memory-corruption trip.
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

	var bad int64
	var mu sync.Mutex
	var msg string
	{
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				// Retain outputs so the GC scans a large live heap concurrent
				// with the encode; a stale pooled pointer then leaves a dangling
				// reference the collector trips over.
				keep := make([][]byte, 0, iters)
				for it := 0; it < iters; it++ {
					out, err := enc.AppendJSON(nil, mk(g, it))
					if err != nil {
						if atomic.AddInt64(&bad, 1) == 1 {
							mu.Lock()
							msg = fmt.Sprintf("g%d i%d err: %v", g, it, err)
							mu.Unlock()
						}
						continue
					}
					if string(out) != golden[g][it] {
						if atomic.AddInt64(&bad, 1) == 1 {
							mu.Lock()
							msg = fmt.Sprintf("g%d i%d cross-contamination:\n want=%s\n  got=%s", g, it, golden[g][it], string(out))
							mu.Unlock()
						}
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
	if bad != 0 {
		t.Fatalf("bad=%d %s", bad, msg)
	}
}

// ---------------------------------------------------------------------------
// Encoder map scratch: the scratch's mapEntries/mapKeyArena/mapIter/valueBacking
// are recycled per call through the pool. These cases vary key kind and value
// pointer content to exercise every scratch slot.
// ---------------------------------------------------------------------------

type ccInner struct {
	X string `json:"x"`
	Y int    `json:"y"`
}

func TestCorruptionEncodeMapStringPtrValues(t *testing.T) {
	type S struct {
		M map[string]*ccInner `json:"m"`
	}
	enc, err := CompileEncoder[S](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
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
	enc, err := CompileEncoder[S](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
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
	enc, err := CompileEncoder[S](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
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
	enc, err := CompileEncoder[S](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
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
	enc, err := CompileEncoder[S](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
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
	enc, err := CompileEncoder[S](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
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

// ---------------------------------------------------------------------------
// Inline catch-all (",inline"): uses the marshaler key scratch slot and the
// value backing, like maps.
// ---------------------------------------------------------------------------

type ccInlineDoc struct {
	ID    int                 `json:"id"`
	Extra map[string]*ccInner `json:",inline"`
}

func TestCorruptionEncodeInline(t *testing.T) {
	enc, err := CompileEncoder[ccInlineDoc](EncoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	runDistinctEncode(t, enc, 16, 3000, func(g, it int) *ccInlineDoc {
		m := map[string]*ccInner{}
		for k := 0; k < 6; k++ {
			tag := fmt.Sprintf("%d_%d_%d", g, it, k)
			m["e"+tag] = &ccInner{X: "x" + tag, Y: g + it + k}
		}
		return &ccInlineDoc{ID: g*1000 + it, Extra: m}
	})
}

// ---------------------------------------------------------------------------
// Custom marshalers: exercise the marshalers[] scratch slots (value box reuse).
// ---------------------------------------------------------------------------

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
	enc, err := CompileEncoder[S](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
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

// ---------------------------------------------------------------------------
// Everything at once: the maximal-scratch document.
// ---------------------------------------------------------------------------

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
	enc, err := CompileEncoder[ccKitchenSink](EncoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
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

// ---------------------------------------------------------------------------
// Top-level Marshal path (global plan cache), realistic public entry point.
// ---------------------------------------------------------------------------

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
	var bad int64
	var mu sync.Mutex
	var msg string
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
						if atomic.AddInt64(&bad, 1) == 1 {
							mu.Lock()
							msg = fmt.Sprintf("g%d i%d err %v", g, it, err)
							mu.Unlock()
						}
						continue
					}
					if string(out) != golden[g][it] {
						if atomic.AddInt64(&bad, 1) == 1 {
							mu.Lock()
							msg = fmt.Sprintf("g%d i%d\n want=%s\n  got=%s", g, it, golden[g][it], string(out))
							mu.Unlock()
						}
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
	if bad != 0 {
		t.Fatalf("bad=%d %s", bad, msg)
	}
}

// ---------------------------------------------------------------------------
// Decoder path under GC pressure: distinct inputs into distinct destinations,
// escaped strings force the string arena; owned mode clones source per string.
// ---------------------------------------------------------------------------

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
	dec, err := CompileDecoder[Doc](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const goroutines = 16
	const iters = 3000
	var bad int64
	var mu sync.Mutex
	var msg string
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
						if atomic.AddInt64(&bad, 1) == 1 {
							mu.Lock()
							msg = fmt.Sprintf("g%d i%d err %v input=%s", g, it, err, input)
							mu.Unlock()
						}
						continue
					}
					if dst.ID != g*100000+it || dst.Name != "name-"+tag+"-☃" ||
						dst.Inner.Tag != "esc\n\t"+tag || len(dst.Inner.Nums) != 2 ||
						dst.Inner.Nums[0] != g || dst.Labels["L"+tag] != "V"+tag ||
						dst.Ptr == nil || dst.Ptr.Tag != "ptr-"+tag {
						if atomic.AddInt64(&bad, 1) == 1 {
							mu.Lock()
							msg = fmt.Sprintf("g%d i%d mismatch: %+v", g, it, dst)
							mu.Unlock()
						}
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
	if bad != 0 {
		t.Fatalf("bad=%d %s", bad, msg)
	}
}

// ---------------------------------------------------------------------------
// Codec round-trip and streaming under GC pressure. Each goroutine owns its
// Writer/Reader; the Codec is shared (contract: safe for concurrent use).
// ---------------------------------------------------------------------------

func TestCorruptionCodecRoundTrip(t *testing.T) {
	type CD struct {
		Name string         `json:"name"`
		Vals map[string]int `json:"vals"`
		List []string       `json:"list"`
	}
	codec, err := CompileCodec[CD](CodecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const goroutines = 16
	const iters = 3000
	var bad int64
	var mu sync.Mutex
	var msg string
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
					enc, err := codec.Marshal(&src)
					if err != nil {
						continue
					}
					var dst CD
					if err := codec.Unmarshal(enc, &dst); err != nil {
						if atomic.AddInt64(&bad, 1) == 1 {
							mu.Lock()
							msg = fmt.Sprintf("dec g%d i%d: %v enc=%s", g, it, err, enc)
							mu.Unlock()
						}
						continue
					}
					if dst.Name != src.Name || dst.Vals["a"+tag] != g || dst.Vals["b"+tag] != it ||
						len(dst.List) != 3 || dst.List[0] != "l1-"+tag {
						if atomic.AddInt64(&bad, 1) == 1 {
							mu.Lock()
							msg = fmt.Sprintf("g%d i%d mismatch: %+v", g, it, dst)
							mu.Unlock()
						}
					}
				}
			}(g)
		}
		wg.Wait()
	}
	if bad != 0 {
		t.Fatalf("bad=%d %s", bad, msg)
	}
}

func TestCorruptionStreaming(t *testing.T) {
	type SV struct {
		K string         `json:"k"`
		N int            `json:"n"`
		M map[string]int `json:"m"`
	}
	codec, err := CompileCodec[SV](CodecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	goroutines := testIterations(14, 8)
	iters := testIterations(1_500, 150)
	const perStream = 5
	var bad int64
	var mu sync.Mutex
	var msg string
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
						if err := codec.EncodeTo(w, &vals[j]); err != nil {
							mu.Lock()
							if msg == "" {
								msg = fmt.Sprintf("enc g%d i%d j%d: %v", g, it, j, err)
							}
							mu.Unlock()
						}
						if err := w.Newline(); err != nil {
							mu.Lock()
							if msg == "" {
								msg = fmt.Sprintf("newline g%d i%d j%d: %v", g, it, j, err)
							}
							mu.Unlock()
						}
					}
					if err := w.Flush(); err != nil {
						mu.Lock()
						if msg == "" {
							msg = fmt.Sprintf("flush g%d i%d: %v", g, it, err)
						}
						mu.Unlock()
					}
					r := NewReader(bytes.NewReader(buf.Bytes()))
					for j := 0; j < perStream; j++ {
						if !r.Next() {
							if atomic.AddInt64(&bad, 1) == 1 {
								mu.Lock()
								msg = fmt.Sprintf("g%d i%d j%d Next=false err=%v", g, it, j, r.Err())
								mu.Unlock()
							}
							break
						}
						var dst SV
						if err := codec.DecodeFrom(r, &dst); err != nil {
							if atomic.AddInt64(&bad, 1) == 1 {
								mu.Lock()
								msg = fmt.Sprintf("g%d i%d j%d dec: %v", g, it, j, err)
								mu.Unlock()
							}
							continue
						}
						tag := fmt.Sprintf("g%d-i%d-j%d", g, it, j)
						if dst.K != vals[j].K || dst.N != vals[j].N || dst.M["m"+tag] != j || len(dst.M) != 1 {
							if atomic.AddInt64(&bad, 1) == 1 {
								mu.Lock()
								msg = fmt.Sprintf("g%d i%d j%d mismatch got=%+v want=%+v", g, it, j, dst, vals[j])
								mu.Unlock()
							}
						}
					}
				}
			}(g)
		}
		wg.Wait()
	}
	if bad != 0 {
		t.Fatalf("bad=%d %s", bad, msg)
	}
}

// ---------------------------------------------------------------------------
// Shared-source readers: Parse/Get, GetRaw, ScanFirstRaw on one read-only buffer.
// ---------------------------------------------------------------------------

func TestCorruptionSharedSourceReaders(t *testing.T) {
	src := []byte(`{"users":[{"id":1,"name":"alice"},{"id":2,"name":"bob"}],` +
		`"meta":{"count":2,"tag":"x"},"deep":{"a":{"b":{"c":"found"}}},"name":"root"}`)
	const goroutines = 16
	const iters = 4000
	var bad int64
	var mu sync.Mutex
	var msg string
	fail := func(format string, a ...any) {
		if atomic.AddInt64(&bad, 1) == 1 {
			mu.Lock()
			msg = fmt.Sprintf(format, a...)
			mu.Unlock()
		}
	}
	{
		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for it := 0; it < iters; it++ {
					v, err := ParseOptions(src, Options{ZeroCopy: true})
					if err != nil {
						fail("g%d i%d parse %v", g, it, err)
						continue
					}
					if name, _ := v.Get("name"); func() string { s, _ := name.Text(); return s }() != "root" {
						fail("g%d i%d name wrong", g, it)
					}
					r1, ok1, e1 := GetRaw(src, "/users/1/name")
					if e1 != nil || !ok1 || string(r1.Bytes()) != `"bob"` {
						fail("g%d i%d GetRaw users/1/name = %q ok=%v err=%v", g, it, string(r1.Bytes()), ok1, e1)
					}
					r2, ok2, e2 := ScanFirstRaw(src, "/deep/a/b/c")
					if e2 != nil || !ok2 || string(r2.Bytes()) != `"found"` {
						fail("g%d i%d ScanFirstRaw deep = %q", g, it, string(r2.Bytes()))
					}
					r3, ok3, e3 := GetRaw(src, "/meta")
					if e3 != nil || !ok3 || string(r3.Bytes()) != `{"count":2,"tag":"x"}` {
						fail("g%d i%d GetRaw meta = %q", g, it, string(r3.Bytes()))
					}
				}
			}(g)
		}
		wg.Wait()
	}
	if bad != 0 {
		t.Fatalf("bad=%d %s", bad, msg)
	}
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
	var bad int64
	var mu sync.Mutex
	var msg string
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
						if atomic.AddInt64(&bad, 1) == 1 {
							mu.Lock()
							msg = fmt.Sprintf("g%d i%d err %v", g, it, err)
							mu.Unlock()
						}
						continue
					}
					want := UD{A: g*100 + it, B: "b-" + tag, C: map[string]int{"x": g, "y": it}, D: []string{"d1-" + tag, "d2-" + tag}}
					if !reflect.DeepEqual(dst, want) {
						if atomic.AddInt64(&bad, 1) == 1 {
							mu.Lock()
							msg = fmt.Sprintf("g%d i%d got=%+v want=%+v", g, it, dst, want)
							mu.Unlock()
						}
					}
				}
			}(g)
		}
		close(start)
		wg.Wait()
	}
	if bad != 0 {
		t.Fatalf("bad=%d %s", bad, msg)
	}
}
