package slopjson

import (
	"fmt"
	"strings"
	"testing"
)

// These benchmarks measure the method-hook win over the compiled interpreter.
// The interpreter path is already the fastest general typed decoder in this
// package, so the hook delta is the ceiling a generated body captures on top of
// it. Each benchmark interleaves the two sub-benchmarks (hook vs interpreter)
// over the identical corpus so a shared A/B ratio survives machine load.
//
//	GOEXPERIMENT=simd gotip test -run x -bench 'BenchmarkHook' -benchmem ./

// hkbRecord is the interpreter twin: a plain struct with no hooks, decoded
// and encoded entirely by the compiled interpreter.
type hkbRecord struct {
	ID     int64   `json:"id"`
	Active bool    `json:"active"`
	Name   string  `json:"name"`
	Note   string  `json:"note"`
	Score  float64 `json:"score"`
}

// hkbHookRecord carries the identical layout with hooks. It is a fresh type
// (not the alloc-test one) so this file stands alone.
type hkbHookRecord struct {
	ID     int64   `json:"id"`
	Active bool    `json:"active"`
	Name   string  `json:"name"`
	Note   string  `json:"note"`
	Score  float64 `json:"score"`
}

var hkbHookFields = MakeFieldSet("id", "active", "name", "note", "score")

func (r *hkbHookRecord) UnmarshalSimdJSON(c DecodeCursor) (DecodeCursor, error) {
	if null, err := c.Null(); err != nil {
		return c, err
	} else if null {
		return c, nil
	}
	if err := c.BeginObject("hkbHookRecord"); err != nil {
		return c, err
	}
	// Expected-order fast path with a general fallback.
	if c.Field(true, hkbHookFields.Field(0)) {
		if err := c.Int64(&r.ID); err != nil {
			return c, err
		}
		if c.Field(false, hkbHookFields.Field(1)) {
			if err := c.Bool(&r.Active); err != nil {
				return c, err
			}
			if c.Field(false, hkbHookFields.Field(2)) {
				if err := c.String(&r.Name); err != nil {
					return c, err
				}
				if c.Field(false, hkbHookFields.Field(3)) {
					if err := c.String(&r.Note); err != nil {
						return c, err
					}
					if c.Field(false, hkbHookFields.Field(4)) {
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
	}
	err := r.unmarshalRest(&c)
	return c, err
}

func (r *hkbHookRecord) unmarshalRest(c *DecodeCursor) error {
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
		idx, known := hkbHookFields.Lookup(key, cs)
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
			err = c.String(&r.Note)
		case 4:
			err = c.Float64(&r.Score)
		}
		if err != nil {
			return err
		}
	}
}

func (r *hkbHookRecord) MarshalSimdJSON(w TrustedAppender) TrustedAppender {
	w = w.RawUnchecked(`{"id":`).Int(r.ID)
	if r.Active {
		w = w.RawUnchecked(`,"active":true`)
	} else {
		w = w.RawUnchecked(`,"active":false`)
	}
	w = w.RawUnchecked(`,"name":`).String(r.Name)
	w = w.RawUnchecked(`,"note":`).String(r.Note)
	w = w.RawUnchecked(`,"score":`).Float64(r.Score)
	return w.RawByteUnchecked('}')
}

type hkbDoc struct {
	Items []hkbRecord `json:"items"`
}

type hkbHookDoc struct {
	Items []hkbHookRecord `json:"items"`
}

func hkbRecordsJSON(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"active":%v,"name":"record-%d","note":"a moderately sized note field number %d","score":%d.75}`,
			i, i%2 == 0, i, i, i)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// BenchmarkHookDecodeLarge interleaves hook vs interpreter decode of a
// 1024-record document. b.ReportAllocs on each sub-benchmark confirms both
// stay allocation-free per record beyond the shared decode entry.
func BenchmarkHookDecodeLarge(b *testing.B) {
	src := hkbRecordsJSON(1024)
	opts := DecoderOptions{ZeroCopy: true}
	plain, err := CompileDecoder[hkbDoc](opts)
	if err != nil {
		b.Fatal(err)
	}
	hooked, err := CompileDecoder[hkbHookDoc](opts)
	if err != nil {
		b.Fatal(err)
	}
	dstPlain := hkbDoc{Items: make([]hkbRecord, 0, 1024)}
	dstHook := hkbHookDoc{Items: make([]hkbHookRecord, 0, 1024)}
	b.Run("interpreter", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for range b.N {
			if err := plain.Decode(src, &dstPlain); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("hook", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for range b.N {
			if err := hooked.Decode(src, &dstHook); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkHookEncodeLarge interleaves hook vs interpreter encode.
func BenchmarkHookEncodeLarge(b *testing.B) {
	src := hkbRecordsJSON(1024)
	plainDec, _ := CompileDecoder[hkbDoc](DecoderOptions{ZeroCopy: true})
	hookDec, _ := CompileDecoder[hkbHookDoc](DecoderOptions{ZeroCopy: true})
	var docPlain hkbDoc
	var docHook hkbHookDoc
	if err := plainDec.Decode(src, &docPlain); err != nil {
		b.Fatal(err)
	}
	if err := hookDec.Decode(src, &docHook); err != nil {
		b.Fatal(err)
	}
	plain, _ := CompileEncoder[hkbDoc](EncoderOptions{})
	hooked, _ := CompileEncoder[hkbHookDoc](EncoderOptions{})
	out, err := plain.AppendJSON(nil, &docPlain)
	if err != nil {
		b.Fatal(err)
	}
	size := int64(len(out))
	b.Run("interpreter", func(b *testing.B) {
		buf := make([]byte, 0, len(out))
		b.SetBytes(size)
		b.ReportAllocs()
		for range b.N {
			buf, _ = plain.AppendJSON(buf[:0], &docPlain)
		}
	})
	b.Run("hook", func(b *testing.B) {
		buf := make([]byte, 0, len(out))
		b.SetBytes(size)
		b.ReportAllocs()
		for range b.N {
			buf, _ = hooked.AppendJSON(buf[:0], &docHook)
		}
	})
}

// BenchmarkHookDecodeSmall interleaves hook vs interpreter decode of a single
// small struct, where dispatch overhead dominates and the hook win is largest.
func BenchmarkHookDecodeSmall(b *testing.B) {
	src := []byte(`{"id":42,"active":true,"name":"small","note":"n","score":3.5}`)
	opts := DecoderOptions{ZeroCopy: true}
	plain, _ := CompileDecoder[hkbRecord](opts)
	hooked, _ := CompileDecoder[hkbHookRecord](opts)
	var dstPlain hkbRecord
	var dstHook hkbHookRecord
	b.Run("interpreter", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if err := plain.Decode(src, &dstPlain); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("hook", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if err := hooked.Decode(src, &dstHook); err != nil {
				b.Fatal(err)
			}
		}
	})
}

var fieldSetLookupSink int

func BenchmarkFieldSetLookup(b *testing.B) {
	set := MakeFieldSet("id", "active", "name", "note", "score")
	for _, test := range []struct {
		name string
		key  string
	}{
		{name: "exact", key: "score"},
		{name: "ascii-folded", key: "SCORE"},
		{name: "unknown", key: "missing"},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				fieldSetLookupSink, _ = set.Lookup(test.key, false)
			}
		})
	}
}

func BenchmarkHookDecodeUnknownField(b *testing.B) {
	src := []byte(`{"unknown":0,"id":42,"active":true,"name":"small","note":"n","score":3.5}`)
	decoder, err := CompileDecoder[hkbHookRecord](DecoderOptions{ZeroCopy: true})
	if err != nil {
		b.Fatal(err)
	}
	var dst hkbHookRecord
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHookEncodeSmall interleaves hook vs interpreter encode of a small
// struct.
func BenchmarkHookEncodeSmall(b *testing.B) {
	plainRec := hkbRecord{ID: 42, Active: true, Name: "small", Note: "n", Score: 3.5}
	hookRec := hkbHookRecord{ID: 42, Active: true, Name: "small", Note: "n", Score: 3.5}
	plain, _ := CompileEncoder[hkbRecord](EncoderOptions{})
	hooked, _ := CompileEncoder[hkbHookRecord](EncoderOptions{})
	buf := make([]byte, 0, 128)
	b.Run("interpreter", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			buf, _ = plain.AppendJSON(buf[:0], &plainRec)
		}
	})
	b.Run("hook", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			buf, _ = hooked.AppendJSON(buf[:0], &hookRec)
		}
	})
}
