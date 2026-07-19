package simdjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

type streamRecord struct {
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Active bool    `json:"active"`
	Score  float64 `json:"score"`
}

func streamRecordAt(i int) streamRecord {
	return streamRecord{
		ID:     i,
		Name:   fmt.Sprintf("user-%d with a name long enough to cross words", i),
		Active: i%2 == 0,
		Score:  float64(i) + 0.5,
	}
}

func TestWriterEncodeNDJSONRoundTrip(t *testing.T) {
	enc, err := CompileEncoder[streamRecord](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w := NewWriterSize(&out, 512) // small threshold to exercise mid-stream flushes
	const rows = 500
	for i := 0; i < rows; i++ {
		v := streamRecordAt(i)
		if err := EncodeTo(w, enc, &v); err != nil {
			t.Fatal(err)
		}
		if err := w.Newline(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != rows {
		t.Fatalf("got %d lines, want %d", len(lines), rows)
	}
	for i, line := range lines {
		var got streamRecord
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if got != streamRecordAt(i) {
			t.Fatalf("line %d: got %+v", i, got)
		}
		want, _ := json.Marshal(streamRecordAt(i))
		if line != string(want) {
			t.Fatalf("line %d: output %s, encoding/json %s", i, line, want)
		}
	}
}

func TestWriterTokensMatchStdlib(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out)
	when := time.Date(2026, 7, 13, 21, 4, 5, 123456789, time.UTC)
	if err := w.BeginObject(); err != nil {
		t.Fatal(err)
	}
	w.Key("id")
	w.Int(-42)
	w.Key("big")
	w.Uint(18446744073709551615)
	w.Key("name")
	w.String("he said \"hi\" & <waved> ")
	w.Key("score")
	w.Float64(12.5)
	w.Key("flags")
	w.BeginArray()
	w.Bool(true)
	w.Null()
	w.Float64(1e30)
	w.EndArray()
	w.Key("empty")
	w.BeginObject()
	w.EndObject()
	w.Key("when")
	w.Time(when)
	w.Key("raw")
	w.RawUnchecked([]byte(`{"pre":"encoded"}`))
	if err := w.EndObject(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	type doc struct {
		ID    int             `json:"id"`
		Big   uint64          `json:"big"`
		Name  string          `json:"name"`
		Score float64         `json:"score"`
		Flags []any           `json:"flags"`
		Empty struct{}        `json:"empty"`
		When  time.Time       `json:"when"`
		Raw   json.RawMessage `json:"raw"`
	}
	want, err := json.Marshal(doc{
		ID: -42, Big: 18446744073709551615, Name: "he said \"hi\" & <waved> ",
		Score: 12.5, Flags: []any{true, nil, 1e30}, When: when,
		Raw: json.RawMessage(`{"pre":"encoded"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Fatalf("token output differs\n got %s\nwant %s", out.String(), want)
	}
}

func TestWriterEscapeHTMLToggle(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out)
	w.SetEscapeHTML(false)
	w.String("<a>&</a>")
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if out.String() != `"<a>&</a>"` {
		t.Fatalf("unescaped output %s", out.String())
	}
}

func TestWriterMisuseErrors(t *testing.T) {
	cases := []struct {
		name string
		use  func(w *Writer) error
	}{
		{"value without key", func(w *Writer) error {
			w.BeginObject()
			return w.Int(1)
		}},
		{"key in array", func(w *Writer) error {
			w.BeginArray()
			return w.Key("k")
		}},
		{"end object in array", func(w *Writer) error {
			w.BeginArray()
			return w.EndObject()
		}},
		{"end array at top", func(w *Writer) error {
			return w.EndArray()
		}},
		{"key after key", func(w *Writer) error {
			w.BeginObject()
			w.Key("a")
			return w.Key("b")
		}},
		{"end object after key", func(w *Writer) error {
			w.BeginObject()
			w.Key("a")
			return w.EndObject()
		}},
		{"two top-level values", func(w *Writer) error {
			w.Int(1)
			return w.Int(2)
		}},
		{"flush mid value", func(w *Writer) error {
			w.BeginObject()
			return w.Flush()
		}},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		w := NewWriter(&out)
		if err := tc.use(w); err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		if w.Err() == nil {
			t.Fatalf("%s: error not sticky", tc.name)
		}
		if err := w.Int(7); err == nil {
			t.Fatalf("%s: writer usable after error", tc.name)
		}
	}
}

type failingWriter struct{ after int }

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, errors.New("sink failed")
	}
	f.after--
	return len(p), nil
}

func TestWriterSinkErrorSticky(t *testing.T) {
	w := NewWriterSize(&failingWriter{after: 0}, 512)
	for i := 0; i < 200; i++ {
		w.String(strings.Repeat("x", 32))
		w.Newline()
	}
	if w.Err() == nil {
		t.Fatal("expected sink error")
	}
	if err := w.Flush(); err == nil {
		t.Fatal("Flush should report the sink error")
	}
}

// chunkReader yields at most chunk bytes per Read to exercise refills.
type chunkReader struct {
	data  []byte
	chunk int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), min(c.chunk, len(c.data)))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func streamFixture(t *testing.T, rows int, sep string) []byte {
	t.Helper()
	var raw bytes.Buffer
	for i := 0; i < rows; i++ {
		line, err := json.Marshal(streamRecordAt(i))
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(line)
		raw.WriteString(sep)
	}
	return raw.Bytes()
}

func TestReaderNDJSONRoundTrip(t *testing.T) {
	dec, err := CompileDecoder[streamRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const rows = 300
	for _, chunk := range []int{1, 3, 7, 64, 4096, 1 << 20} {
		for _, sep := range []string{"\n", "", " ", "\r\n\t"} {
			data := streamFixture(t, rows, sep)
			r := newSizedReader(&chunkReader{data: data, chunk: chunk}, 512)
			count := 0
			for r.Next() {
				var got streamRecord
				if err := DecodeFrom(r, dec, &got); err != nil {
					t.Fatalf("chunk=%d sep=%q row %d: %v", chunk, sep, count, err)
				}
				if got != streamRecordAt(count) {
					t.Fatalf("chunk=%d sep=%q row %d: got %+v", chunk, sep, count, got)
				}
				count++
			}
			if r.Err() != nil {
				t.Fatalf("chunk=%d sep=%q: %v", chunk, sep, r.Err())
			}
			if count != rows {
				t.Fatalf("chunk=%d sep=%q: decoded %d of %d", chunk, sep, count, rows)
			}
		}
	}
}

func TestReaderValueLargerThanBuffer(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	data := []byte(`{"name":"` + big + `"}` + "\n" + `{"name":"after"}` + "\n")
	r := newSizedReader(&chunkReader{data: data, chunk: 777}, 512)
	type doc struct {
		Name string `json:"name"`
	}
	dec, _ := CompileDecoder[doc](DecoderOptions{})
	var got doc
	if !r.Next() {
		t.Fatalf("first value: %v", r.Err())
	}
	if err := DecodeFrom(r, dec, &got); err != nil || len(got.Name) != len(big) {
		t.Fatalf("big value: %v len=%d", err, len(got.Name))
	}
	if !r.Next() {
		t.Fatalf("second value: %v", r.Err())
	}
	if err := DecodeFrom(r, dec, &got); err != nil || got.Name != "after" {
		t.Fatalf("after value: %v %+v", err, got)
	}
	if r.Next() || r.Err() != nil {
		t.Fatalf("expected clean end, err=%v", r.Err())
	}
}

func TestReaderErrors(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		r := NewReader(strings.NewReader(`{"a":1}` + "\n" + `{"a":`))
		if !r.Next() {
			t.Fatalf("first value: %v", r.Err())
		}
		if r.Next() {
			t.Fatal("truncated value must not be delivered")
		}
		if r.Err() == nil {
			t.Fatal("expected truncation error")
		}
	})
	t.Run("invalid", func(t *testing.T) {
		r := NewReader(strings.NewReader(`{"a":1}{"b":tru}` + "\n"))
		if !r.Next() {
			t.Fatalf("first value: %v", r.Err())
		}
		if r.Next() {
			t.Fatal("invalid value must not be delivered")
		}
		if r.Err() == nil || !strings.Contains(r.Err().Error(), "offset 7") {
			t.Fatalf("expected positioned error, got %v", r.Err())
		}
	})
	t.Run("whitespace only", func(t *testing.T) {
		r := NewReader(strings.NewReader(" \n\t \r\n"))
		if r.Next() {
			t.Fatal("no values expected")
		}
		if r.Err() != nil {
			t.Fatalf("clean end expected, got %v", r.Err())
		}
	})
	t.Run("empty", func(t *testing.T) {
		r := NewReader(strings.NewReader(""))
		if r.Next() || r.Err() != nil {
			t.Fatalf("clean end expected, got %v", r.Err())
		}
	})
	t.Run("value size limit", func(t *testing.T) {
		big := `{"name":"` + strings.Repeat("x", 10_000) + `"}`
		r := newConfiguredReader(strings.NewReader(big), 512, 2048)
		if r.Next() {
			t.Fatal("oversized value must not be delivered")
		}
		if r.Err() == nil || !strings.Contains(r.Err().Error(), "limit") {
			t.Fatalf("expected limit error, got %v", r.Err())
		}
	})
	t.Run("read error", func(t *testing.T) {
		r := NewReader(io.MultiReader(strings.NewReader(`{"a":1}`+"\n"), &failingReader{}))
		if !r.Next() {
			t.Fatalf("first value: %v", r.Err())
		}
		if r.Next() {
			t.Fatal("no second value expected")
		}
		if r.Err() == nil || !strings.Contains(r.Err().Error(), "broken pipe") {
			t.Fatalf("expected read error, got %v", r.Err())
		}
	})
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestReaderScalarsAndConcatenated(t *testing.T) {
	r := NewReader(strings.NewReader(`1 "two" true null [3,4]{"five":5}` + " 6.5"))
	var got []string
	for r.Next() {
		got = append(got, string(r.Bytes()))
	}
	if r.Err() != nil {
		t.Fatal(r.Err())
	}
	want := []string{`1`, `"two"`, `true`, `null`, `[3,4]`, `{"five":5}`, `6.5`}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("value %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestReaderInputOffset(t *testing.T) {
	data := `{"a":1}` + "\n" + `{"bb":22}` + "\n"
	r := newSizedReader(strings.NewReader(data), 512)
	if !r.Next() {
		t.Fatal(r.Err())
	}
	if r.InputOffset() != 7 {
		t.Fatalf("offset after first value = %d", r.InputOffset())
	}
	if !r.Next() {
		t.Fatal(r.Err())
	}
	if r.InputOffset() != int64(len(data)-1) {
		t.Fatalf("offset after second value = %d", r.InputOffset())
	}
}

func TestStreamWriterReaderPipe(t *testing.T) {
	enc, _ := CompileEncoder[streamRecord](EncoderOptions{})
	dec, _ := CompileDecoder[streamRecord](DecoderOptions{})
	var buf bytes.Buffer
	w := NewWriterSize(&buf, 1024)
	const rows = 200
	for i := 0; i < rows; i++ {
		v := streamRecordAt(i)
		if err := EncodeTo(w, enc, &v); err != nil {
			t.Fatal(err)
		}
		w.Newline()
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := newSizedReader(&chunkReader{data: buf.Bytes(), chunk: 199}, 512)
	count := 0
	for r.Next() {
		var got streamRecord
		if err := DecodeFrom(r, dec, &got); err != nil {
			t.Fatal(err)
		}
		if got != streamRecordAt(count) {
			t.Fatalf("row %d: %+v", count, got)
		}
		count++
	}
	if r.Err() != nil || count != rows {
		t.Fatalf("count=%d err=%v", count, r.Err())
	}
}

func TestStreamSteadyStateAllocs(t *testing.T) {
	enc, _ := CompileEncoder[streamRecord](EncoderOptions{})
	w := NewWriter(io.Discard)
	v := streamRecordAt(7)
	// Warm the buffer, then require allocation-free writes.
	for i := 0; i < 4; i++ {
		if err := EncodeTo(w, enc, &v); err != nil {
			t.Fatal(err)
		}
		w.Newline()
	}
	allocs := testing.AllocsPerRun(200, func() {
		if err := EncodeTo(w, enc, &v); err != nil {
			t.Fatal(err)
		}
		w.Newline()
	})
	if allocs != 0 {
		t.Fatalf("writer allocations per record = %v, want 0", allocs)
	}

	data := streamFixture(t, 400, "\n")
	dec, _ := CompileDecoder[streamRecord](DecoderOptions{ZeroCopy: true})
	reads := testing.AllocsPerRun(20, func() {
		r := newSizedReader(bytes.NewReader(data), 64<<10)
		var got streamRecord
		for r.Next() {
			if err := DecodeFrom(r, dec, &got); err != nil {
				t.Fatal(err)
			}
		}
		if r.Err() != nil {
			t.Fatal(r.Err())
		}
	})
	// One reader buffer plus its bookkeeping per run; nothing per value.
	if reads > 4 {
		t.Fatalf("reader allocations per full stream = %v, want a constant few", reads)
	}
}

func BenchmarkStreamWriteNDJSON(b *testing.B) {
	enc, _ := CompileEncoder[streamRecord](EncoderOptions{})
	records := make([]streamRecord, 512)
	for i := range records {
		records[i] = streamRecordAt(i)
	}
	var bytesPerOp int64
	{
		var probe bytes.Buffer
		w := NewWriter(&probe)
		for i := range records {
			EncodeTo(w, enc, &records[i])
			w.Newline()
		}
		w.Close()
		bytesPerOp = int64(probe.Len())
	}
	b.SetBytes(bytesPerOp)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		w := NewWriter(io.Discard)
		for i := range records {
			if err := EncodeTo(w, enc, &records[i]); err != nil {
				b.Fatal(err)
			}
			w.Newline()
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamWriteNDJSONStdlib(b *testing.B) {
	records := make([]streamRecord, 512)
	for i := range records {
		records[i] = streamRecordAt(i)
	}
	var probe bytes.Buffer
	stdenc := json.NewEncoder(&probe)
	for i := range records {
		stdenc.Encode(&records[i])
	}
	b.SetBytes(int64(probe.Len()))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		enc := json.NewEncoder(io.Discard)
		for i := range records {
			if err := enc.Encode(&records[i]); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkStreamReadNDJSON(b *testing.B) {
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
		for r.Next() {
			if err := DecodeFrom(r, dec, &got); err != nil {
				b.Fatal(err)
			}
		}
		if r.Err() != nil {
			b.Fatal(r.Err())
		}
	}
}

func BenchmarkStreamReadNDJSONStdlib(b *testing.B) {
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
	b.SetBytes(int64(data.Len()))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		dec := json.NewDecoder(bytes.NewReader(data.Bytes()))
		var got streamRecord
		for {
			if err := dec.Decode(&got); err == io.EOF {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
	}
}

// fixedChunkReader delivers data in fixed-size pieces, modeling a value that
// arrives across many small network reads.
type fixedChunkReader struct {
	data  []byte
	pos   int
	chunk int
}

func (r *fixedChunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

// TestStreamReaderLinearOnChunkedValue guards against the O(N^2) re-scan that
// re-framed a large value from its start on every refill. A 16 MiB value split
// into 512-byte chunks is scanned once now (tens of milliseconds); the old
// quadratic path re-scans ~2.7e11 bytes, minutes of work. The bound is set far
// above the linear time yet far below the quadratic one so it stays a clean
// regression signal even under -race (which slows the run roughly tenfold) on a
// loaded machine, rather than a flaky wall-clock assertion.
func TestStreamReaderLinearOnChunkedValue(t *testing.T) {
	const size = 16 << 20
	str := `"` + strings.Repeat("a", size) + `"`
	obj := "[" + strings.Repeat("1,", size/2) + "1]"

	for _, tc := range []struct {
		name string
		data string
	}{
		{"string", str},
		{"array", obj},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			r := NewReader(&fixedChunkReader{data: []byte(tc.data), chunk: 512})
			if !r.Next() || r.Err() != nil {
				t.Fatalf("Next failed: ok=%v err=%v", r.hasValue, r.Err())
			}
			if len(r.Bytes()) != len(tc.data) {
				t.Fatalf("framed %d bytes, want %d", len(r.Bytes()), len(tc.data))
			}
			if elapsed := time.Since(start); elapsed > 30*time.Second {
				t.Fatalf("framing a %d-byte value in 512-byte chunks took %v; the quadratic re-scan regressed", len(tc.data), elapsed)
			}
		})
	}
}
