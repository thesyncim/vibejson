package simdjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// scriptedReader plays back a fixed sequence of Read results. Each step
func TestWriterFlushDoesNotFrameValues(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out)
	if err := w.Int(1); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	err := w.Int(2)
	if err == nil {
		t.Fatal("second top-level value after Flush was accepted; adjacent numbers would merge")
	}
	if strings.Contains(err.Error(), "Flush") {
		t.Fatalf("guard error advertises Flush, which does not frame values: %v", err)
	}
}

// EncodeTo participates in the token layer's top-level framing: mixing a
// token-built scalar with EncodeTo must error rather than merge two numbers
// into one value.
func TestWriterEncodeToBypassesTopLevelGuard(t *testing.T) {
	enc, err := CompileEncoder[int](EncoderOptions{})
	if err != nil {
		t.Skipf("scalar root encoder unavailable: %v", err)
	}
	var out bytes.Buffer
	w := NewWriter(&out)
	if err := w.Int(1); err != nil {
		t.Fatal(err)
	}
	two := 2
	errEnc := EncodeTo(w, enc, &two)
	errTok := w.Int(3) // started was reset by EncodeTo, so this is accepted too
	if err := w.Flush(); err != nil && errEnc == nil && errTok == nil {
		t.Fatal(err)
	}
	if errEnc == nil && errTok == nil {
		r := NewReader(bytes.NewReader(out.Bytes()))
		got := collectValues(r)
		t.Errorf("token value + EncodeTo + token value produced no error and wrote %q, which reads back as %d value(s) %q instead of 3 values 1, 2, 3",
			out.String(), len(got), got)
	}
}

// Non-finite floats must error like Marshal, and the error must be sticky.
func TestWriterNonFiniteFloats(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		var out bytes.Buffer
		w := NewWriter(&out)
		if err := w.Float64(v); err == nil {
			t.Fatalf("Float(%v) must error like Marshal", v)
		}
		if w.Err() == nil {
			t.Fatalf("Float(%v): error not sticky", v)
		}
		if err := w.Int(1); err == nil {
			t.Fatalf("Float(%v): writer usable after error", v)
		}
	}
}

// String must match Marshal byte for byte, including invalid UTF-8
// replacement, control escapes, and U+2028/U+2029.
func TestWriterStringParity(t *testing.T) {
	cases := []string{
		"",
		"\xff",
		"a\xffb\xfe",
		string([]byte{0xed, 0xa0, 0x80}), // lone surrogate bytes
		"\x00\x1f\x7f",
		"héllo wörld",
		" line sep",
		"<script>alert(1)&</script>",
		strings.Repeat("é", 300) + "\"quote\\back",
		"tab\tnl\ncr\rbs\bff\f",
		strings.Repeat("clean ascii ", 100),
	}
	for _, escape := range []bool{true, false} {
		for _, s := range cases {
			var out bytes.Buffer
			w := NewWriter(&out)
			w.SetEscapeHTML(escape)
			if err := w.String(s); err != nil {
				t.Fatalf("escape=%v %q: %v", escape, s, err)
			}
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
			var wantBuf bytes.Buffer
			stdenc := json.NewEncoder(&wantBuf)
			stdenc.SetEscapeHTML(escape)
			if err := stdenc.Encode(s); err != nil {
				t.Fatal(err)
			}
			want := strings.TrimSuffix(wantBuf.String(), "\n")
			if out.String() != want {
				t.Errorf("escape=%v input %q:\n got %s\nwant %s", escape, s, out.String(), want)
			}
		}
	}
}

// Integer emitters at the boundaries.
func TestWriterIntegerBoundaries(t *testing.T) {
	ints := []int64{math.MinInt64, math.MinInt64 + 1, -1, 0, 1, math.MaxInt64}
	for _, v := range ints {
		var out bytes.Buffer
		w := NewWriter(&out)
		if err := w.Int(v); err != nil {
			t.Fatal(err)
		}
		w.Flush()
		if want := strconv.FormatInt(v, 10); out.String() != want {
			t.Errorf("Int(%d) = %s, want %s", v, out.String(), want)
		}
	}
	uints := []uint64{0, 1, math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64}
	for _, v := range uints {
		var out bytes.Buffer
		w := NewWriter(&out)
		if err := w.Uint(v); err != nil {
			t.Fatal(err)
		}
		w.Flush()
		if want := strconv.FormatUint(v, 10); out.String() != want {
			t.Errorf("Uint(%d) = %s, want %s", v, out.String(), want)
		}
	}
}

// Float spelling parity with Marshal on boundary values.
func TestWriterFloatParity(t *testing.T) {
	values := []float64{
		math.Copysign(0, -1), 0, 0.1, -0.1, 1e-6, 5e-324, 1e15, 1e15 - 2,
		1e20, 1e21, 1.5e21, math.MaxFloat64, -math.MaxFloat64, 2.2250738585072014e-308,
	}
	for _, v := range values {
		var out bytes.Buffer
		w := NewWriter(&out)
		if err := w.Float64(v); err != nil {
			t.Fatal(err)
		}
		w.Flush()
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if out.String() != string(want) {
			t.Errorf("Float(%v) = %s, want %s", v, out.String(), want)
		}
	}
}

// Time parity with Marshal, including the prefix/date cache paths (repeated
// second, same day, changed zone) and the out-of-range year error.
func TestWriterTimeParity(t *testing.T) {
	base := time.Date(2026, 7, 14, 12, 0, 0, 987654321, time.UTC)
	zone := time.FixedZone("probe", 5*3600+1800)
	times := []time.Time{
		base,
		base,                      // same second: prefix cache path
		base.Add(3 * time.Second), // same day: date cache path
		base.In(zone),             // same absolute second, different zone
		base.Add(26 * time.Hour),  // next day
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
	}
	var out bytes.Buffer
	w := NewWriter(&out) // one writer so the internal TimeCache carries across values
	for i, tm := range times {
		out.Reset()
		w.buf = w.buf[:0]
		if err := w.Time(tm); err != nil {
			t.Fatalf("time %d (%v): %v", i, tm, err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		w.started = false // fresh top-level slot without disturbing the cache
		want, err := json.Marshal(tm)
		if err != nil {
			t.Fatal(err)
		}
		if out.String() != string(want) {
			t.Errorf("time %d (%v): got %s, want %s", i, tm, out.String(), want)
		}
	}

	var out2 bytes.Buffer
	w2 := NewWriter(&out2)
	if err := w2.Time(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("year 10000 must error like Marshal")
	}
	if err := w2.Int(1); err == nil {
		t.Fatal("time error not sticky")
	}
}

// Close with unclosed containers must error, for both kinds.
func TestWriterCloseUnfinishedValue(t *testing.T) {
	for _, open := range []func(w *Writer) error{(*Writer).BeginObject, (*Writer).BeginArray} {
		var out bytes.Buffer
		w := NewWriter(&out)
		if err := open(w); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err == nil {
			t.Fatal("Close with an unclosed container must error")
		}
		if w.Err() == nil {
			t.Fatal("Close error not sticky")
		}
	}
}

// shortWriteSink violates the io.Writer contract by accepting one byte per
// call with a nil error; the Writer must convert that to io.ErrShortWrite
// rather than dropping the tail.
type shortWriteSink struct{ got []byte }

func (s *shortWriteSink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.got = append(s.got, p[0])
	return 1, nil
}

func TestWriterShortWriteSink(t *testing.T) {
	sink := &shortWriteSink{}
	w := NewWriterSize(sink, 512)
	if err := w.String("hello world"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Close = %v, want io.ErrShortWrite", err)
	}
	if w.Err() == nil {
		t.Fatal("short write not sticky")
	}
}

// A sink error must surface at Close even when nothing crossed the flush
// threshold beforehand — no silent loss of a buffered value.
func TestWriterSinkErrorSurfacesAtClose(t *testing.T) {
	w := NewWriter(&failingWriter{after: 0}) // default 32K threshold: no mid-stream flush
	if err := w.String("buffered"); err != nil {
		t.Fatal(err)
	}
	if err := w.Newline(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close must report the sink error")
	}
}

// Values whose ends straddle the flush threshold at many alignments, with
// escapes and multi-byte runes near the boundary: output must match
// encoding/json exactly and flushes must never split inside a value.
type recordingSink struct {
	bytes.Buffer
	writes []int
}

func (s *recordingSink) Write(p []byte) (int, error) {
	s.writes = append(s.writes, len(p))
	return s.Buffer.Write(p)
}

func TestWriterFlushBoundaryEscapes(t *testing.T) {
	sink := &recordingSink{}
	w := NewWriterSize(sink, 512)
	var want bytes.Buffer
	stdenc := json.NewEncoder(&want)
	for i := 0; i < 300; i++ {
		s := strings.Repeat("é", i%7) + "\"\\<& " + strings.Repeat("x", i%13) + "\xff"
		if err := w.String(s); err != nil {
			t.Fatal(err)
		}
		if err := w.Newline(); err != nil {
			t.Fatal(err)
		}
		if err := stdenc.Encode(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sink.Bytes(), want.Bytes()) {
		t.Fatalf("output diverges from encoding/json across %d flushes", len(sink.writes))
	}
	// Every value written must also read back intact.
	r := newSizedReader(bytes.NewReader(sink.Bytes()), 512)
	count := 0
	for r.Next() {
		count++
	}
	if r.Err() != nil || count != 300 {
		t.Fatalf("re-read: %d values, err=%v", count, r.Err())
	}
}

// Writer output (tokens, EncodeTo, Raw mixed) re-read by Reader must yield
// the same values byte for byte.
func TestWriterReaderRoundTripMixed(t *testing.T) {
	enc, err := CompileEncoder[streamContractRecord](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := NewWriterSize(&out, 512)
	var want []string
	for i := 0; i < 50; i++ {
		switch i % 3 {
		case 0:
			v := streamContractRecord{A: i}
			if err := EncodeTo(w, enc, &v); err != nil {
				t.Fatal(err)
			}
			want = append(want, fmt.Sprintf(`{"a":%d}`, i))
		case 1:
			w.BeginObject()
			w.Key("a")
			w.Int(int64(i))
			if err := w.EndObject(); err != nil {
				t.Fatal(err)
			}
			want = append(want, fmt.Sprintf(`{"a":%d}`, i))
		default:
			raw := fmt.Sprintf(`{"a":%d}`, i)
			if err := w.RawUnchecked([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			want = append(want, raw)
		}
		if err := w.Newline(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := newSizedReader(&chunkReader{data: out.Bytes(), chunk: 7}, 512)
	got := collectValues(r)
	if r.Err() != nil {
		t.Fatal(r.Err())
	}
	if len(got) != len(want) {
		t.Fatalf("got %d values, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("value %d: got %q want %q", i, got[i], want[i])
		}
	}
}
