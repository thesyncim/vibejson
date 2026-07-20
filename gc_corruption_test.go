package simdjson

// GC-corruption second pass.
//
// This file extends concurrency_corruption_test.go with cases aimed squarely at
// the garbage collector: a heap object that transiently holds a pointer into a
// goroutine stack corrupts the heap only when a stack move relocates that stack
// mid-use and a GC then scans the stale copy. Map sources now remain visible to
// escape analysis and pooled iterators are explicitly unbound; these tests add
// an in-process stack-move driver and cover streaming one-pass and zero-copy
// paths that also thread source pointers through unsafe.
//
// As with the sibling file, -race MASKS this class by perturbing scheduling, so
// the intended stress invocation is without -race:
//
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestGC -count=5 -cpu=1,4,8 ./
//
// The smoke form (plain, low count) is safe for CI.

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// forceStackMovement recurses to depth then does token work, guaranteeing the
// caller's goroutine stack has grown (and thus been relocated by the runtime's
// copystack) at least once. It returns a value derived from the whole frame so
// the compiler cannot elide the recursion. Interleaving this with encoding on
// the same goroutine reproduces the precise condition the map-iterator fix
// guards: a stack move while a heap iterator references a stack-built map.
func forceStackMovement(depth int, acc int) int {
	if depth == 0 {
		var buf [64]byte
		for i := range buf {
			buf[i] = byte(acc + i)
		}
		s := 0
		for _, b := range buf {
			s += int(b)
		}
		return s
	}
	return forceStackMovement(depth-1, acc+depth) ^ depth
}

// TestGCCorruptionMapStackMove drives stack-built map encoding on goroutines
// that also force stack growth and trigger GC between iterations. Each goroutine
// alternates: build a map on the stack, encode it, force a stack move, and every
// so often run a GC so the collector scans the (now relocated) stacks and the
// heap-resident pooled iterators. A lifetime regression surfaces as a fatal
// "found bad pointer in Go heap" during one of those GCs, or as a value
// mismatch. It complements TestCorruptionCanonicalMapPtr by making the stack
// move happen in-process rather than relying only on -cpu transitions.
func TestGCCorruptionMapStackMove(t *testing.T) {
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
	const goroutines = 12
	const iters = 4000
	var wg sync.WaitGroup
	var bad int64
	var sink int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			keep := make([][]byte, 0, 2048)
			for it := 0; it < iters; it++ {
				// Build the map in this small, preemptible frame so its value
				// pointer is stack-resident when the iterator binds to it.
				m := map[string]*Inner{}
				for k := 0; k < 6; k++ {
					m[fmt.Sprintf("k%d_%d_%d", g, it, k)] = &Inner{X: fmt.Sprintf("x%d_%d", g, it), Y: it + k}
				}
				out, err := enc.AppendJSON(nil, &S{M: m})
				if err != nil {
					atomic.AddInt64(&bad, 1)
					continue
				}
				// Grow and relocate this goroutine's stack immediately after the
				// encode, while the pooled iterator (returned to the pool) still
				// carries whatever pointer it bound to.
				atomic.AddInt64(&sink, int64(forceStackMovement(24+(it&31), it)))
				want := fmt.Sprintf(`"x%d_%d"`, g, it)
				if !strings.Contains(string(out), want) {
					atomic.AddInt64(&bad, 1)
				}
				keep = append(keep, out)
				if len(keep) > 1500 {
					keep = keep[750:]
					runtime.GC()
				}
			}
			_ = keep
		}(g)
	}
	wg.Wait()
	if bad != 0 {
		t.Fatalf("bad=%d: corruption or contamination (sink=%d)", bad, atomic.LoadInt64(&sink))
	}
}

// TestGCCorruptionDecodeNextMapValues exercises the one-pass streaming decode
// path (DecodeNext + the resumable value framer) on structs carrying maps, which
// runs the string-body SIMD framer and the map-value decode over a rolling
// buffer under concurrency and GC pressure. Distinct per-goroutine payloads are
// retained so the collector scans a large live heap while other goroutines
// decode; every field is checked, catching both fatal corruption and silent
// cross-goroutine contamination.
func TestGCCorruptionDecodeNextMapValues(t *testing.T) {
	type SV struct {
		K string            `json:"k"`
		N int               `json:"n"`
		M map[string]string `json:"m"`
	}
	enc, err := CompileEncoder[SV](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := CompileDecoder[SV](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const goroutines = 12
	const iters = 1500
	const perStream = 6
	var wg sync.WaitGroup
	var failures corruptionFailures
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			keep := make([]SV, 0, 4096)
			for it := 0; it < iters; it++ {
				var buf bytes.Buffer
				w := NewWriter(&buf)
				want := make([]SV, perStream)
				for j := 0; j < perStream; j++ {
					tag := fmt.Sprintf("g%d-i%d-j%d", g, it, j)
					// A long string value forces the framer's SIMD string-body
					// scan across vector strides and buffer refills.
					val := SV{
						K: "k-" + tag,
						N: g*100000 + it*10 + j,
						M: map[string]string{"m" + tag: strings.Repeat(tag+".", 8)},
					}
					want[j] = val
					if err := EncodeTo(w, enc, &val); err != nil {
						failures.record(fmt.Sprintf("enc g%d i%d j%d: %v", g, it, j, err))
					}
					_ = w.Newline()
				}
				_ = w.Flush()

				r := newSizedReader(bytes.NewReader(buf.Bytes()), 64)
				for j := 0; j < perStream; j++ {
					var dst SV
					if !DecodeNext(r, dec, &dst) {
						failures.record(fmt.Sprintf("g%d i%d j%d DecodeNext=false err=%v", g, it, j, r.Err()))
						break
					}
					tag := fmt.Sprintf("g%d-i%d-j%d", g, it, j)
					if dst.K != want[j].K || dst.N != want[j].N ||
						len(dst.M) != 1 || dst.M["m"+tag] != strings.Repeat(tag+".", 8) {
						failures.record(fmt.Sprintf("g%d i%d j%d mismatch got=%+v want=%+v", g, it, j, dst, want[j]))
					}
					keep = append(keep, dst)
				}
				if len(keep) > 3000 {
					keep = keep[1500:]
					runtime.GC()
				}
			}
			_ = keep
		}(g)
	}
	wg.Wait()
	failures.requireNone(t)
}

// TestGCCorruptionStreamGrowthUnderGC pushes the Reader's buffer through many
// grow-and-compact cycles while retaining every decoded (owned) value and
// forcing GCs, so any pointer the framer or decoder holds into a superseded
// backing array would be caught as corruption or as a stale read. Values vary in
// size to drive both buffer growth (large values) and compaction (a large value
// split across the initial small buffer).
func TestGCCorruptionStreamGrowthUnderGC(t *testing.T) {
	// Build a stream whose value sizes swing widely so the reader's buffer both
	// grows past its initial size and compacts partial prefixes repeatedly.
	var stream bytes.Buffer
	type rec struct {
		idx int
		s   string
	}
	var expect []rec
	for i := 0; i < 4000; i++ {
		size := 4 + (i*37)%900
		s := fmt.Sprintf("v%d-%s", i, strings.Repeat("abcdefgh", size/8+1))
		fmt.Fprintf(&stream, `{"idx":%d,"s":%q}`+"\n", i, s)
		expect = append(expect, rec{idx: i, s: s})
	}
	raw := stream.Bytes()

	type Doc struct {
		Idx int    `json:"idx"`
		S   string `json:"s"`
	}
	// Owned decoder (copies out of the buffer): retained values must stay valid
	// across buffer growth and GC.
	dec, err := CompileDecoder[Doc](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 8
	var wg sync.WaitGroup
	var failures corruptionFailures
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// Start from a tiny buffer so nearly every value triggers a grow or
			// compact.
			r := newSizedReader(bytes.NewReader(raw), 64)
			kept := make([]Doc, 0, len(expect))
			i := 0
			for DecodeNext(r, dec, new(Doc)) {
				var d Doc
				if err := DecodeFrom(r, dec, &d); err != nil {
					failures.record(fmt.Sprintf("g%d decodeTo %d: %v", g, i, err))
					break
				}
				if d.Idx != expect[i].idx || d.S != expect[i].s {
					failures.record(fmt.Sprintf("g%d value %d mismatch got=%+v", g, i, d))
					break
				}
				kept = append(kept, d)
				if i%500 == 0 {
					runtime.GC()
				}
				i++
			}
			if err := r.Err(); err != nil {
				failures.record(fmt.Sprintf("g%d err: %v", g, err))
			}
			if i != len(expect) {
				failures.record(fmt.Sprintf("g%d got %d values want %d", g, i, len(expect)))
			}
			runtime.GC()
			// Re-verify retained values after a final GC: a dangling backing
			// pointer would show as corruption here.
			for k := range kept {
				if kept[k].Idx != expect[k].idx || kept[k].S != expect[k].s {
					failures.record(fmt.Sprintf("g%d retained %d corrupted", g, k))
					break
				}
			}
		}(g)
	}
	wg.Wait()
	failures.requireNone(t)
}
