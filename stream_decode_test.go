package simdjson

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodePrefixConcatenated(t *testing.T) {
	dec, err := CompileDecoder[streamRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	one, _ := json.Marshal(streamRecordAt(1))
	two, _ := json.Marshal(streamRecordAt(2))
	src := []byte("  " + string(one) + " \n\t" + string(two) + "trailing-garbage-left-alone")
	var v streamRecord
	n, err := dec.DecodePrefix(src, &v)
	if err != nil || v != streamRecordAt(1) {
		t.Fatalf("first: n=%d err=%v v=%+v", n, err, v)
	}
	rest := src[n:]
	m, err := dec.DecodePrefix(rest, &v)
	if err != nil || v != streamRecordAt(2) {
		t.Fatalf("second: err=%v v=%+v", err, v)
	}
	if !strings.HasPrefix(string(rest[m:]), "trailing-garbage") {
		t.Fatalf("consumed too much: %q", rest[m:])
	}
	// Truncated input must error, not panic.
	if _, err := dec.DecodePrefix(one[:len(one)-3], &v); err == nil {
		t.Fatal("truncated prefix must error")
	}
}

func TestDecodeNextErrors(t *testing.T) {
	dec, _ := CompileDecoder[streamRecord](DecoderOptions{})
	t.Run("type error surfaces without draining", func(t *testing.T) {
		// The mistyped value is followed by much more input; the error must
		// surface once the value is complete, not after reading the rest.
		tail := strings.Repeat(`{"id":1}`+"\n", 100_000)
		r := newSizedReader(strings.NewReader(`{"id":"nope"}`+"\n"+tail), 512)
		var got streamRecord
		if DecodeNext(r, dec, &got) {
			t.Fatal("mistyped value must not decode")
		}
		if r.Err() == nil || !strings.Contains(r.Err().Error(), "offset 0") {
			t.Fatalf("expected positioned decode error, got %v", r.Err())
		}
	})
	t.Run("truncated", func(t *testing.T) {
		r := NewReader(strings.NewReader(`{"id":1}` + "\n" + `{"id":`))
		var got streamRecord
		if !DecodeNext(r, dec, &got) {
			t.Fatalf("first value: %v", r.Err())
		}
		if DecodeNext(r, dec, &got) {
			t.Fatal("truncated value must not decode")
		}
		if r.Err() == nil {
			t.Fatal("expected truncation error")
		}
	})
	t.Run("clean end", func(t *testing.T) {
		r := NewReader(strings.NewReader(`{"id":1}` + "\n \t"))
		var got streamRecord
		if !DecodeNext(r, dec, &got) || got.ID != 1 {
			t.Fatalf("first value: %v %+v", r.Err(), got)
		}
		if DecodeNext(r, dec, &got) || r.Err() != nil {
			t.Fatalf("clean end expected, err=%v", r.Err())
		}
	})
}

func BenchmarkStreamDecodeNextNDJSON(b *testing.B) {
	var data bytes.Buffer
	{
		enc, _ := CompileEncoder[streamRecord](EncoderOptions{})
		w := NewWriter(&data)
		for i := 0; i < 512; i++ {
			v := streamRecordAt(i)
			EncodeTo(w, enc, &v)
			w.Newline()
		}
		w.Close()
	}
	dec, _ := CompileDecoder[streamRecord](DecoderOptions{ZeroCopy: true})
	b.SetBytes(int64(data.Len()))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r := NewReader(bytes.NewReader(data.Bytes()))
		var got streamRecord
		for DecodeNext(r, dec, &got) {
		}
		if r.Err() != nil {
			b.Fatal(r.Err())
		}
	}
}
