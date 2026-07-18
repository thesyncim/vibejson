package simdjson

// Dynamic-interface corruption pass.
//
// Dynamic decoding constructs all scalar interfaces through ordinary Go
// conversions. This file stresses the resulting lifetime boundary: many
// goroutines decode concurrently, force stack growth and GC between iterations,
// retain trees across collections, and re-verify every retained tree at the
// end. A violation surfaces as a fatal collector error or as a tree that no
// longer DeepEquals the encoding/json ground truth.
//
// The intended collector stress invocation is without -race:
//
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestGCCorruptionAnyBox -count=5 -cpu=1,4,8 ./
//
// The smoke form (plain, low count) is safe for CI.

import (
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// anyBoxCorpusDoc builds a document that hits every dynamic interface kind:
// float64s in and out of the plain-integer fast path, strings clean and
// escaped, nested non-empty arrays, empty arrays, and objects, salted so
// goroutines cannot share results by accident.
const anyBoxCorpusRows = 4300

func anyBoxCorpusDoc(salt int) []byte {
	var b strings.Builder
	b.Grow(640 << 10)
	fmt.Fprintf(&b, `{"salt":%d,"empty":[],"rows":[`, salt)
	for i := 0; i < anyBoxCorpusRows; i++ {
		if i != 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b,
			`{"id":%d,"f":%d.%04d,"e":1.5e%d,"s":"row-%d-%d","esc":"a\tbé%d","xs":[%d,%d.5,-3e2],"none":[],"ok":%t,"gap":null}`,
			salt+i, i, i*7%9973, i%30, salt, i, i, i, i, i%2 == 0)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// TestGCCorruptionDynamicAnyValues decodes documents on goroutines that force
// stack relocation and GC between iterations, retaining trees across many
// collections. Owned-mode sources are scribbled over after decoding: a value
// that still aliased caller storage would surface as a mismatch, not just a
// leak. Every retained tree is re-verified after the churn.
func TestGCCorruptionDynamicAnyValues(t *testing.T) {
	ownedDecoder, err := CompileDecoder[any](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	zeroCopyDecoder, err := CompileDecoder[any](DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	const goroutines = 12
	iters := 24
	if testing.Short() {
		iters = 4
	}
	var wg sync.WaitGroup
	var bad int64
	var sink int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			type kept struct {
				got  any
				want any
			}
			keep := make([]kept, 0, 6)
			for it := 0; it < iters; it++ {
				src := anyBoxCorpusDoc(g*1_000_000 + it)
				var want any
				if err := json.Unmarshal(src, &want); err != nil {
					atomic.AddInt64(&bad, 1)
					continue
				}
				zeroCopy := it%2 == 1
				decoder := ownedDecoder
				if zeroCopy {
					decoder = zeroCopyDecoder
				}
				var got any
				if err := decoder.Decode(src, &got); err != nil {
					atomic.AddInt64(&bad, 1)
					continue
				}
				if !zeroCopy {
					// Owned results must not alias src in any way.
					for i := range src {
						src[i] = 'X'
					}
				}
				// Relocate this goroutine's stack while the fresh tree's slab
				// chunks are only reachable through its interface values.
				atomic.AddInt64(&sink, int64(forceStackMovement(24+(it&31), it)))
				if !reflect.DeepEqual(got, want) {
					atomic.AddInt64(&bad, 1)
					continue
				}
				if zeroCopy {
					// Zero-copy trees alias src, which this loop rewrites next
					// iteration by building a fresh one; retain only owned trees.
					continue
				}
				keep = append(keep, kept{got: got, want: want})
				if len(keep) >= 6 {
					runtime.GC()
					for i := range keep {
						if !reflect.DeepEqual(keep[i].got, keep[i].want) {
							atomic.AddInt64(&bad, 1)
						}
					}
					keep = keep[:0]
				}
			}
			runtime.GC()
			for i := range keep {
				if !reflect.DeepEqual(keep[i].got, keep[i].want) {
					atomic.AddInt64(&bad, 1)
				}
			}
		}(g)
	}
	wg.Wait()
	if bad != 0 {
		t.Fatalf("bad=%d: slab-boxed trees diverged from encoding/json (sink=%d)", bad, atomic.LoadInt64(&sink))
	}
}

// TestDynamicAnyUsesRuntimeInterfaces pins the centralized conversion contract.
func TestDynamicAnyUsesRuntimeInterfaces(t *testing.T) {
	var parser parser
	if value, ok := parser.boxAnyFloat64(1.25).(float64); !ok || value != 1.25 {
		t.Fatalf("float boxing = %v, %v", value, ok)
	}
	if value, ok := parser.boxAnyString("safe", false).(string); !ok || value != "safe" {
		t.Fatalf("string boxing = %q, %v", value, ok)
	}
	want := []any{"value", float64(2)}
	value, ok := parser.boxAnySlice(want).([]any)
	if !ok || !reflect.DeepEqual(value, want) {
		t.Fatalf("slice boxing = %#v, %v", value, ok)
	}
}
