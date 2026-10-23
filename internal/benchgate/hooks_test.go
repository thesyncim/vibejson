package benchgate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	vibejson "github.com/thesyncim/vibejson"
)

// These fixtures and benchmark bodies mirror typed_hook_bench_test.go. Keeping
// the harness outside the library test package excludes unrelated test code
// from the benchmark binary; bench-gate copies this package to both revisions.
type hkbRecord struct {
	ID     int64   `json:"id"`
	Active bool    `json:"active"`
	Name   string  `json:"name"`
	Note   string  `json:"note"`
	Score  float64 `json:"score"`
}

type hkbHookRecord struct {
	ID     int64   `json:"id"`
	Active bool    `json:"active"`
	Name   string  `json:"name"`
	Note   string  `json:"note"`
	Score  float64 `json:"score"`
}

var hkbHookFields = vibejson.MakeFieldSet("id", "active", "name", "note", "score")

func (r *hkbHookRecord) UnmarshalVibeJSON(c vibejson.DecodeCursor) (vibejson.DecodeCursor, error) {
	if null, err := c.Null(); err != nil {
		return c, err
	} else if null {
		return c, nil
	}
	if err := c.BeginObject("hkbHookRecord"); err != nil {
		return c, err
	}
	if c.Field(true, hkbHookFields.Field(0)) {
		if err := c.Int(&r.ID); err != nil {
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
						if err := c.Float(&r.Score); err != nil {
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

func (r *hkbHookRecord) unmarshalRest(c *vibejson.DecodeCursor) error {
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
			err = c.Int(&r.ID)
		case 1:
			err = c.Bool(&r.Active)
		case 2:
			err = c.String(&r.Name)
		case 3:
			err = c.String(&r.Note)
		case 4:
			err = c.Float(&r.Score)
		}
		if err != nil {
			return err
		}
	}
}

func (r *hkbHookRecord) MarshalVibeJSON(w vibejson.TrustedAppender) vibejson.TrustedAppender {
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

func BenchmarkHookDecodeLarge(b *testing.B) {
	src := hkbRecordsJSON(1024)
	opts := vibejson.DecoderOptions{ZeroCopy: true}
	plain, err := vibejson.CompileDecoder[hkbDoc](opts)
	if err != nil {
		b.Fatal(err)
	}
	hooked, err := vibejson.CompileDecoder[hkbHookDoc](opts)
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

func BenchmarkHookDecodeSmall(b *testing.B) {
	src := []byte(`{"id":42,"active":true,"name":"small","note":"n","score":3.5}`)
	opts := vibejson.DecoderOptions{ZeroCopy: true}
	plain, _ := vibejson.CompileDecoder[hkbRecord](opts)
	hooked, _ := vibejson.CompileDecoder[hkbHookRecord](opts)
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

func TestHookDecodeParityAndReuse(t *testing.T) {
	opts := vibejson.DecoderOptions{ZeroCopy: true}
	small, err := vibejson.CompileDecoder[hkbHookRecord](opts)
	if err != nil {
		t.Fatal(err)
	}
	large, err := vibejson.CompileDecoder[hkbHookDoc](opts)
	if err != nil {
		t.Fatal(err)
	}
	var gotSmall, wantSmall hkbHookRecord
	gotLarge := hkbHookDoc{Items: make([]hkbHookRecord, 0, 1024)}
	wantLarge := hkbHookDoc{Items: make([]hkbHookRecord, 0, 1024)}
	for round := 0; round < 3; round++ {
		for _, src := range [][]byte{
			[]byte(`{"id":42,"active":true,"name":"small","note":"n","score":3.5}`),
			[]byte(`{"score":-2.25,"note":"é\n","name":"different","active":false,"id":7}`),
			[]byte(`{"unknown":[{"nested":true}],"id":9,"active":true,"name":"unknown first","note":"n","score":1.25}`),
			[]byte(`null`),
		} {
			if err := json.Unmarshal(src, &wantSmall); err != nil {
				t.Fatal(err)
			}
			if err := small.Decode(src, &gotSmall); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotSmall, wantSmall) {
				t.Fatalf("small %s: got %#v, want %#v", src, gotSmall, wantSmall)
			}
		}
		for _, count := range []int{1024, 3, 0, 1024} {
			src := hkbRecordsJSON(count)
			if err := json.Unmarshal(src, &wantLarge); err != nil {
				t.Fatal(err)
			}
			if err := large.Decode(src, &gotLarge); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotLarge, wantLarge) {
				t.Fatalf("large count %d differs from encoding/json", count)
			}
		}
	}
}

func TestHookDecodeErrorsAndRecovery(t *testing.T) {
	decoder, err := vibejson.CompileDecoder[hkbHookDoc](vibejson.DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	var got hkbHookDoc
	valid := hkbRecordsJSON(3)
	var want hkbHookDoc
	if err := json.Unmarshal(valid, &want); err != nil {
		t.Fatal(err)
	}
	for _, src := range [][]byte{
		[]byte(`{"items":[{"id":"wrong"}]}`),
		[]byte(`{"items":[{"id":1,"active":tru}]}`),
		[]byte(`{"items":[{"id":1,"active":true,"name":"unterminated}]}`),
		append(hkbRecordsJSON(3), 'x'),
	} {
		var oracle hkbHookDoc
		if err := json.Unmarshal(src, &oracle); err == nil {
			t.Fatalf("test input unexpectedly valid: %s", src)
		}
		if err := decoder.Decode(src, &got); err == nil {
			t.Fatalf("hook decode accepted malformed input: %s", src)
		}
		if err := decoder.Decode(valid, &got); err != nil {
			t.Fatalf("decode after error: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("decode after error differs from encoding/json")
		}
	}
}
