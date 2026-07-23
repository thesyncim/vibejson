package slopjson

import "testing"

// hookAllocRecord is a small flat type whose hooks read and write only scalars,
// isolating the dispatch cost from container allocation so the 0-alloc claim is
// unambiguous.
type hookAllocRecord struct {
	ID     int64   `json:"id"`
	Active bool    `json:"active"`
	Name   string  `json:"name"`
	Score  float64 `json:"score"`
}

var hookAllocFields = MakeFieldSet("id", "active", "name", "score")

func (r *hookAllocRecord) UnmarshalSimdJSON(c DecodeCursor) (DecodeCursor, error) {
	if null, err := c.Null(); err != nil {
		return c, err
	} else if null {
		return c, nil
	}
	if err := c.BeginObject("hookAllocRecord"); err != nil {
		return c, err
	}
	if c.Field(true, hookAllocFields.Field(0)) {
		if err := c.Int64(&r.ID); err != nil {
			return c, err
		}
		if c.Field(false, hookAllocFields.Field(1)) {
			if err := c.Bool(&r.Active); err != nil {
				return c, err
			}
			if c.Field(false, hookAllocFields.Field(2)) {
				if err := c.String(&r.Name); err != nil {
					return c, err
				}
				if c.Field(false, hookAllocFields.Field(3)) {
					if err := c.Float64(&r.Score); err != nil {
						return c, err
					}
					if c.ExpectObjectClose() {
						return c, nil
					}
				}
			}
		}
	}
	err := r.unmarshalRest(&c)
	return c, err
}

func (r *hookAllocRecord) unmarshalRest(c *DecodeCursor) error {
	cs := c.CaseSensitive()
	first := true
	for {
		key, ok, err := c.NextField(first)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		first = false
		idx, known := hookAllocFields.Lookup(key, cs)
		if !known {
			if err := c.Skip(); err != nil {
				return err
			}
			continue
		}
		switch idx {
		case 0:
			err = c.Int64(&r.ID)
		case 1:
			err = c.Bool(&r.Active)
		case 2:
			err = c.String(&r.Name)
		case 3:
			err = c.Float64(&r.Score)
		}
		if err != nil {
			return err
		}
	}
}

func (r *hookAllocRecord) MarshalSimdJSON(w TrustedAppender) TrustedAppender {
	w = w.RawUnchecked(`{"id":`).Int(r.ID)
	w = w.RawUnchecked(`,"active":`).Bool(r.Active)
	w = w.RawUnchecked(`,"name":`).String(r.Name)
	w = w.RawUnchecked(`,"score":`).Float64(r.Score)
	return w.RawByteUnchecked('}')
}

// TestHookDecodeAllocationBound guards the by-value hook dispatch against
// receiver boxing or cursor-state escapes. Reused zero-copy decode must remain
// allocation-free, just like the compiled interpreter.
func TestHookDecodeAllocationBound(t *testing.T) {
	hookDec, err := CompileDecoder[hookAllocRecord](DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	plainDec, err := CompileDecoder[hookAllocRecordPlain](DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`{"id":42,"active":true,"name":"short","score":3.5}`)
	var hookDst hookAllocRecord
	var plainDst hookAllocRecordPlain
	// Warm up (plan lookups, first-call setup) outside the measured runs.
	if err := hookDec.Decode(src, &hookDst); err != nil {
		t.Fatal(err)
	}
	if err := plainDec.Decode(src, &plainDst); err != nil {
		t.Fatal(err)
	}
	hookAllocs := testing.AllocsPerRun(500, func() {
		if err := hookDec.Decode(src, &hookDst); err != nil {
			t.Fatal(err)
		}
	})
	plainAllocs := testing.AllocsPerRun(500, func() {
		if err := plainDec.Decode(src, &plainDst); err != nil {
			t.Fatal(err)
		}
	})
	if hookAllocs != 0 || hookAllocs != plainAllocs {
		t.Fatalf("hook decode allocated %v/op vs interpreter %v/op, want both 0", hookAllocs, plainAllocs)
	}
}

var hookAllocSink hookAllocRecord

//go:noinline
func decodeLocalHookRecord(dec Decoder[hookAllocRecord], src []byte) error {
	var dst hookAllocRecord
	if err := dec.Decode(src, &dst); err != nil {
		return err
	}
	hookAllocSink = dst
	return nil
}

// TestHookDecodeLocalDestinationAllocationBound covers the stack-eligible
// entry shape, not only a destination reused by the caller. Passing its address
// through a hook must not force heap shadows per field or cursor. One ordinary
// Go escape is allowed because an arbitrary pointer-receiver method may retain
// *T; hiding that possibility from escape analysis would be unsafe.
func TestHookDecodeLocalDestinationAllocationBound(t *testing.T) {
	dec, err := CompileDecoder[hookAllocRecord](DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`{"id":42,"active":true,"name":"short","score":3.5}`)
	if err := decodeLocalHookRecord(dec, src); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(500, func() {
		if err := decodeLocalHookRecord(dec, src); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 1 {
		t.Fatalf("local hook decode allocated %v/op, want <=1 receiver-lifetime escape", allocs)
	}
}

// TestHookEncodeAllocationBound proves that an addressable encode receiver is
// exposed through an ordinary GC-visible interface without a detached shadow
// or allocation while the TrustedAppender reuses caller-owned output storage.
func TestHookEncodeAllocationBound(t *testing.T) {
	enc, err := CompileEncoder[hookAllocRecord](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	src := hookAllocRecord{ID: 42, Active: true, Name: "short", Score: 3.5}
	buf := make([]byte, 0, 128)
	out, err := enc.AppendJSON(buf, &src)
	if err != nil {
		t.Fatal(err)
	}
	_ = out
	allocs := testing.AllocsPerRun(200, func() {
		var err error
		buf, err = enc.AppendJSON(buf[:0], &src)
		if err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("hook encode allocated %v/op, want 0", allocs)
	}
}

// encodeLocalHookArray deliberately creates addressable source storage in its
// own frame. Hook dispatch may expose element pointers to user code, so the
// compiler must retain this array as one operation-lifetime object; the number
// of elements must not turn that single escape into per-hook allocations.
//
//go:noinline
func encodeLocalHookArray(enc Encoder[[8]hookAllocRecord], dst []byte) ([]byte, error) {
	var src [8]hookAllocRecord
	for i := range src {
		src[i] = hookAllocRecord{ID: int64(i), Active: i&1 == 0, Name: "local", Score: float64(i)}
	}
	return enc.AppendJSON(dst, &src)
}

func TestHookEncodeLocalSourceAllocationBound(t *testing.T) {
	enc, err := CompileEncoder[[8]hookAllocRecord](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, 768)
	buf, err = encodeLocalHookArray(enc, buf)
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(200, func() {
		buf, err = encodeLocalHookArray(enc, buf[:0])
		if err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 1 {
		t.Fatalf("eight local encode hooks allocated %v/op, want <=1 operation-lifetime source escape", allocs)
	}
}

// TestHookNonUserAllocationBound is the A/B that a type WITHOUT hooks is unaffected:
// its decode and encode allocate exactly as the plain reflection path does,
// with no per-call cost introduced by the hook machinery. hookAllocRecord's
// plain twin has the identical layout but no hooks.
type hookAllocRecordPlain struct {
	ID     int64   `json:"id"`
	Active bool    `json:"active"`
	Name   string  `json:"name"`
	Score  float64 `json:"score"`
}

func TestHookNonUserAllocationBound(t *testing.T) {
	dec, err := CompileDecoder[hookAllocRecordPlain](DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := CompileEncoder[hookAllocRecordPlain](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`{"id":42,"active":true,"name":"short","score":3.5}`)
	var dst hookAllocRecordPlain
	if err := dec.Decode(src, &dst); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, 128)
	if _, err := enc.AppendJSON(buf, &dst); err != nil {
		t.Fatal(err)
	}
	// A non-hook type keeps its established allocation profile: one shared
	// decode-entry allocation and a fully in-place encode. If the hook
	// machinery had leaked cost into the common path, these would grow.
	decAllocs := testing.AllocsPerRun(200, func() {
		if err := dec.Decode(src, &dst); err != nil {
			t.Fatal(err)
		}
	})
	if decAllocs > 1 {
		t.Fatalf("non-hook decode allocated %v/op, want <=1 (hook machinery must be zero-cost to non-users)", decAllocs)
	}
	encAllocs := testing.AllocsPerRun(200, func() {
		var err error
		buf, err = enc.AppendJSON(buf[:0], &dst)
		if err != nil {
			t.Fatal(err)
		}
	})
	if encAllocs != 0 {
		t.Fatalf("non-hook encode allocated %v/op, want 0", encAllocs)
	}
}
