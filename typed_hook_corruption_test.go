package vibejson

import (
	stdjson "encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

type hookCorruptRecord struct {
	ID    int64         `json:"id"`
	Name  string        `json:"name"`
	Addr  hookAddress   `json:"addr"`
	Kids  []hookAddress `json:"kids"`
	Score float64       `json:"score"`
}

var hookCorruptFields = MakeFieldSet("id", "name", "addr", "kids", "score")

func (r *hookCorruptRecord) UnmarshalVibeJSON(c DecodeCursor) (DecodeCursor, error) {
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
			err = c.Int(&r.ID)
		case 1:
			err = c.String(&r.Name)
		case 2:
			var next DecodeCursor
			next, err = r.Addr.UnmarshalVibeJSON(c)
			c = next
		case 3:
			err = r.decodeKids(&c)
		case 4:
			err = c.Float(&r.Score)
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
		next, err := a.UnmarshalVibeJSON(*c)
		*c = next
		if err != nil {
			return err
		}
		r.Kids = append(r.Kids, a)
	}
}

func (r *hookCorruptRecord) MarshalVibeJSON(w TrustedAppender) TrustedAppender {
	w = w.RawUnchecked(`{"id":`).Int(r.ID)
	w = w.RawUnchecked(`,"name":`).String(r.Name)
	w = w.RawUnchecked(`,"addr":`)
	w = r.Addr.MarshalVibeJSON(w)
	w = w.RawUnchecked(`,"kids":`)
	if r.Kids == nil {
		w = w.Null()
	} else {
		w = w.RawByteUnchecked('[')
		for i := range r.Kids {
			if i > 0 {
				w = w.RawByteUnchecked(',')
			}
			w = r.Kids[i].MarshalVibeJSON(w)
		}
		w = w.RawByteUnchecked(']')
	}
	w = w.RawUnchecked(`,"score":`).Float64(r.Score)
	return w.RawByteUnchecked('}')
}

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

				var viaHook hookCorruptRecord
				if err := hookDec.Decode(doc, &viaHook); err != nil {
					fail("hook decode error: " + err.Error())
					continue
				}
				var viaPlain hookCorruptRecordPlain
				if err := plainDec.Decode(doc, &viaPlain); err != nil {
					fail("plain decode error: " + err.Error())
					continue
				}
				if !hookCorruptEqual(viaHook, viaPlain) {
					fail(fmt.Sprintf("g%d it%d decode mismatch: hook=%+v plain=%+v", g, it, viaHook, viaPlain))
					continue
				}

				out, err := hookEnc.AppendJSON(nil, &viaHook)
				if err != nil {
					fail("hook encode error: " + err.Error())
					continue
				}
				atomic.AddInt64(&sink, int64(forceStackMovement(24+(it&31), it)))

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

type gcReceiverPayload struct {
	tag  uint64
	fill [256]byte
}

const gcReceiverTag = 0x5144_4a53_4d49_53

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

func (p *gcReceiverProbe) UnmarshalVibeJSON(c DecodeCursor) (DecodeCursor, error) {
	if err := c.Skip(); err != nil {
		return c, err
	}
	p.payload = newGCReceiverPayload()
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
