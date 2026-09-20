package vibejson

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

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
				m := map[string]*Inner{}
				for k := 0; k < 6; k++ {
					m[fmt.Sprintf("k%d_%d_%d", g, it, k)] = &Inner{X: fmt.Sprintf("x%d_%d", g, it), Y: it + k}
				}
				out, err := enc.AppendJSON(nil, &S{M: m})
				if err != nil {
					atomic.AddInt64(&bad, 1)
					continue
				}
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

func TestGCCorruptionStreamGrowthUnderGC(t *testing.T) {
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
