package simdjson

import (
	stdjson "encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// This file is the corruption gate for the method-hook tier. DecodeCursor state
// crosses the hook by value; decode and encode receivers use ordinary
// GC-visible pointers. The collector must see every reference while goroutine
// stacks move and calls run concurrently.
// These tests drive that lifetime contract under aggressive GC:
//
//	GOGC=1 GOEXPERIMENT=simd gotip test -run TestHookCorruption -count=5 -cpu=1,4,8 ./
//
// A regression surfaces as a fatal "found bad pointer in Go heap" / "found
// pointer to free object" crash during a GC, or as a silent value mismatch
// (cross-goroutine bleed, or a stale receiver).

// hookCorruptRecord carries scalars, a string that outlives the frame, a nested
// hooked struct, and a slice of nested hooks, so decode exercises every hook
// dispatch site (scalar, nested struct, array element) and encode exercises the
// by-value TrustedAppender through all of them.
type hookCorruptRecord struct {
	ID    int64         `json:"id"`
	Name  string        `json:"name"`
	Addr  hookAddress   `json:"addr"`
	Kids  []hookAddress `json:"kids"`
	Score float64       `json:"score"`
}

var hookCorruptFields = MakeFieldSet("id", "name", "addr", "kids", "score")

func (r *hookCorruptRecord) UnmarshalSimdJSON(c DecodeCursor) (DecodeCursor, error) {
	if null, err := c.Null(); err != nil {
		return c, err
	} else if null {
		return c, nil
	}
	if err := c.BeginObject("hookCorruptRecord"); err != nil {
		return c, err
	}
	first := true
	cs := c.CaseSensitive()
	for {
		key, ok, err := c.NextField(first)
		if err != nil {
			return c, err
		}
		if !ok {
			return c, nil
		}
		first = false
		idx, known := hookCorruptFields.Lookup(key, cs)
		if !known {
			if err := c.Skip(); err != nil {
				return c, err
			}
			continue
		}
		switch idx {
		case 0:
			err = c.Int64(&r.ID)
		case 1:
			err = c.String(&r.Name)
		case 2:
			var next DecodeCursor
			next, err = r.Addr.UnmarshalSimdJSON(c)
			c = next
		case 3:
			err = r.decodeKids(&c)
		case 4:
			err = c.Float64(&r.Score)
		}
		if err != nil {
			return c, err
		}
	}
}

func (r *hookCorruptRecord) decodeKids(c *DecodeCursor) error {
	if null, err := c.Null(); err != nil {
		return err
	} else if null {
		r.Kids = nil
		return nil
	}
	if err := c.BeginArray("[]hookAddress"); err != nil {
		return err
	}
	if r.Kids == nil {
		r.Kids = []hookAddress{}
	} else {
		r.Kids = r.Kids[:0]
	}
	first := true
	for {
		more, err := c.NextElement(first)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		first = false
		var a hookAddress
		next, err := a.UnmarshalSimdJSON(*c)
		*c = next
		if err != nil {
			return err
		}
		r.Kids = append(r.Kids, a)
	}
}

func (r *hookCorruptRecord) MarshalSimdJSON(w TrustedAppender) TrustedAppender {
	w = w.RawUnchecked(`{"id":`).Int(r.ID)
	w = w.RawUnchecked(`,"name":`).String(r.Name)
	w = w.RawUnchecked(`,"addr":`)
	w = r.Addr.MarshalSimdJSON(w)
	w = w.RawUnchecked(`,"kids":`)
	if r.Kids == nil {
		w = w.Null()
	} else {
		w = w.RawByteUnchecked('[')
		for i := range r.Kids {
			if i > 0 {
				w = w.RawByteUnchecked(',')
			}
			w = r.Kids[i].MarshalSimdJSON(w)
		}
		w = w.RawByteUnchecked(']')
	}
	w = w.RawUnchecked(`,"score":`).Float64(r.Score)
	return w.RawByteUnchecked('}')
}

// hookCorruptRecordPlain is the reflection-path twin used as the oracle.
type hookCorruptRecordPlain struct {
	ID    int64              `json:"id"`
	Name  string             `json:"name"`
	Addr  hookAddressPlain   `json:"addr"`
	Kids  []hookAddressPlain `json:"kids"`
	Score float64            `json:"score"`
}

func hookCorruptDoc(g, it int) []byte {
	return []byte(fmt.Sprintf(
		`{"id":%d,"name":"n-%d-%d","addr":{"street":"s-%d-%d","city":"c-%d","zip":%d},`+
			`"kids":[{"street":"k0-%d","city":"kc-%d","zip":%d},{"street":"k1-%d","city":"kc2-%d","zip":%d}],`+
			`"score":%d.5}`,
		g*100000+it, g, it, g, it, g, it, g, it, it+1, g, it, it+2, it))
}

// TestHookCorruptionConcurrentDecodeEncode drives concurrent decode AND encode
// of hook-implementing types across many goroutines, forcing stack movement and
// GC between iterations, and checks every result against the reflection path.
// A bad-pointer crash, a stale receiver, or a cross-goroutine bleed fails it.
func TestHookCorruptionConcurrentDecodeEncode(t *testing.T) {
	hookDec, err := CompileDecoder[hookCorruptRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hookEnc, err := CompileEncoder[hookCorruptRecord](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plainDec, err := CompileDecoder[hookCorruptRecordPlain](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 12
	const iters = 3000
	var wg sync.WaitGroup
	var failures corruptionFailures
	var sink int64
	fail := failures.recordSticky

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			keep := make([][]byte, 0, 2048)
			for it := 0; it < iters; it++ {
				doc := hookCorruptDoc(g, it)

				// Decode through the hook path in a small, preemptible frame so
				// any stack-derived pointer the dispatch laundered is live when
				// the stack next moves.
				var viaHook hookCorruptRecord
				if err := hookDec.Decode(doc, &viaHook); err != nil {
					fail("hook decode error: " + err.Error())
					continue
				}
				// Oracle: reflection-path decode of the identical document.
				var viaPlain hookCorruptRecordPlain
				if err := plainDec.Decode(doc, &viaPlain); err != nil {
					fail("plain decode error: " + err.Error())
					continue
				}
				if !hookCorruptEqual(viaHook, viaPlain) {
					fail(fmt.Sprintf("g%d it%d decode mismatch: hook=%+v plain=%+v", g, it, viaHook, viaPlain))
					continue
				}

				// Encode through the hook path, then force a stack relocation
				// while the (returned) TrustedAppender's laundered buffer pointer and
				// the pooled decoder state are still around.
				out, err := hookEnc.AppendJSON(nil, &viaHook)
				if err != nil {
					fail("hook encode error: " + err.Error())
					continue
				}
				atomic.AddInt64(&sink, int64(forceStackMovement(24+(it&31), it)))

				// The encoded bytes must round-trip to the same value and match
				// encoding/json, proving no receiver/buffer bled between calls.
				want, err := stdjson.Marshal(&viaPlain)
				if err != nil {
					fail("std marshal error: " + err.Error())
					continue
				}
				if string(out) != string(want) {
					fail(fmt.Sprintf("g%d it%d encode mismatch:\n hook=%s\n  std=%s", g, it, out, want))
					continue
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
	if failures.bad != 0 {
		t.Fatalf("bad=%d, first=%q (sink=%d)", failures.bad, failures.msg, atomic.LoadInt64(&sink))
	}
}

func hookCorruptEqual(a hookCorruptRecord, b hookCorruptRecordPlain) bool {
	if a.ID != b.ID || a.Name != b.Name || a.Score != b.Score {
		return false
	}
	if hookAddressPlain(a.Addr) != b.Addr {
		return false
	}
	if len(a.Kids) != len(b.Kids) {
		return false
	}
	for i := range a.Kids {
		if hookAddressPlain(a.Kids[i]) != b.Kids[i] {
			return false
		}
	}
	return true
}

// gcReceiverPayload is a heap object reachable only through the hook receiver
// during the body. It holds a filled buffer with a sentinel so a
// collect-and-reuse of its memory shows up as a mismatch.
type gcReceiverPayload struct {
	tag  uint64
	fill [256]byte
}

const gcReceiverTag = 0x5144_4a53_4d49_53

// gcReceiverProbe proves the ordinary addressable receiver is a GC-visible root
// for the whole call. The body allocates a payload reachable
// only through the receiver, drops every other reference, forces several GCs
// with intervening allocation, and re-reads the payload through the receiver.
// If dispatch failed to keep the receiver visible to the collector, the
// payload could be swept and reused and the re-read would not match the
// sentinel.
type gcReceiverProbe struct {
	payload *gcReceiverPayload
	ok      bool
}

func newGCReceiverPayload() *gcReceiverPayload {
	p := &gcReceiverPayload{tag: gcReceiverTag}
	for i := range p.fill {
		p.fill[i] = byte(i)
	}
	return p
}

func (p *gcReceiverProbe) UnmarshalSimdJSON(c DecodeCursor) (DecodeCursor, error) {
	if err := c.Skip(); err != nil {
		return c, err
	}
	// Reachable only through the receiver from here on.
	p.payload = newGCReceiverPayload()
	// Force collections with heap churn between them, so a payload the GC
	// cannot see through the receiver would be reclaimed and its memory reused.
	for k := 0; k < 3; k++ {
		runtime.GC()
		churn := make([][]byte, 64)
		for i := range churn {
			churn[i] = make([]byte, 512)
		}
		runtime.KeepAlive(churn)
	}
	pl := p.payload
	good := pl.tag == gcReceiverTag
	for i := range pl.fill {
		if pl.fill[i] != byte(i) {
			good = false
			break
		}
	}
	p.ok = good
	return c, nil
}

// TestHookGCReceiverVisibility proves the receiver is scanned and kept alive
// for the whole call, so a payload
// reachable only through the receiver survives collections during the body.
func TestHookGCReceiverVisibility(t *testing.T) {
	dec, err := CompileDecoder[gcReceiverProbe](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		var p gcReceiverProbe
		if err := dec.Decode([]byte(`{"x":1}`), &p); err != nil {
			t.Fatal(err)
		}
		if !p.ok {
			t.Fatalf("iter %d: receiver-reachable payload was corrupted across GC", i)
		}
	}
}
