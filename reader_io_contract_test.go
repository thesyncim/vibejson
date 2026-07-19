package simdjson

// Contract tests for streaming API edge cases: pathological io.Readers, Writer
// state machine and emitter parity, DecodeNext error handling, and configured
// value-limit boundaries. Each test pins an invariant that once failed; the
// fixtures are the minimized reproductions.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// scriptedReader plays back a fixed sequence of Read results. Each step
type scriptStep struct {
	data []byte
	err  error
}

type scriptedReader struct {
	steps    []scriptStep
	finalErr error
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if len(r.steps) == 0 {
		if r.finalErr != nil {
			return 0, r.finalErr
		}
		return 0, io.EOF
	}
	step := r.steps[0]
	n := copy(p, step.data)
	if n < len(step.data) {
		r.steps[0].data = step.data[n:]
		return n, nil
	}
	r.steps = r.steps[1:]
	return n, step.err
}

func newSizedReader(in io.Reader, size int) *Reader {
	return newConfiguredReader(in, size, 0)
}

func newConfiguredReader(in io.Reader, size, maxValueBytes int) *Reader {
	r, err := NewReaderWithOptions(in, ReaderOptions{
		BufferSize:    size,
		MaxValueBytes: maxValueBytes,
	})
	if err != nil {
		panic(err)
	}
	return r
}

func collectValues(r *Reader) []string {
	var got []string
	for r.Next() {
		got = append(got, string(r.Bytes()))
	}
	return got
}

// --- Attack surface 1: pathological io.Readers ---------------------------

// Values arriving in the same Read call as a non-EOF error must be
// delivered before the error surfaces: the io.Reader contract puts the
// n > 0 bytes first, and encoding/json's Decoder (the baseline below)
// behaves that way.
func TestReaderValueArrivingWithSameReadError(t *testing.T) {
	boom := errors.New("boom")
	payload := []byte(`{"a":1}` + "\n" + `{"a":2}` + "\n")

	// Baseline: encoding/json delivers both values, then reports the error.
	std := json.NewDecoder(&scriptedReader{steps: []scriptStep{{data: payload, err: boom}}, finalErr: boom})
	stdValues := 0
	var stdErr error
	for {
		var v map[string]any
		if err := std.Decode(&v); err != nil {
			stdErr = err
			break
		}
		stdValues++
	}
	if stdValues != 2 || stdErr != boom {
		t.Fatalf("stdlib baseline: %d values, err %v", stdValues, stdErr)
	}

	r := newSizedReader(&scriptedReader{steps: []scriptStep{{data: payload, err: boom}}, finalErr: boom}, 512)
	got := collectValues(r)
	if len(got) != 2 {
		t.Errorf("values arriving in the same Read as a non-EOF error were dropped: got %d values %q, want 2", len(got), got)
	}
	if !errors.Is(r.Err(), boom) {
		t.Errorf("Err() = %v, want %v", r.Err(), boom)
	}
}

// The same delivery rule holds when the erroring Read completes a value
// split across earlier calls, including io.ErrUnexpectedEOF as the error.
func TestReaderValueArrivingWithLaterReadError(t *testing.T) {
	for _, tail := range []error{errors.New("boom"), io.ErrUnexpectedEOF} {
		r := newSizedReader(&scriptedReader{steps: []scriptStep{
			{data: []byte(`{"a":1}` + "\n" + `{"a`)},
			{data: []byte(`":2}` + "\n"), err: tail},
		}, finalErr: tail}, 512)
		got := collectValues(r)
		if len(got) != 2 {
			t.Errorf("err=%v: got %d values %q, want 2 (second value completed by the erroring Read)", tail, len(got), got)
		}
		if !errors.Is(r.Err(), tail) {
			t.Errorf("Err() = %v, want %v", r.Err(), tail)
		}
	}
}

// A reader may return (0, nil) any number of times; the Reader must neither
// error spuriously nor lose data. (bufio gives up after 100 with
// io.ErrNoProgress; this Reader retries indefinitely, which also means a
// reader that returns (0, nil) forever would spin — same as io.Copy.)
func TestReaderZeroByteNilReads(t *testing.T) {
	payload := `{"a":1}` + "\n" + `{"a":2}` + "\n"
	var steps []scriptStep
	for i := 0; i < 300; i++ { // more than bufio's 100 tolerance
		steps = append(steps, scriptStep{})
	}
	for i := 0; i < len(payload); i++ {
		steps = append(steps, scriptStep{}, scriptStep{data: []byte{payload[i]}}, scriptStep{})
	}
	r := newSizedReader(&scriptedReader{steps: steps}, 512)
	got := collectValues(r)
	if r.Err() != nil {
		t.Fatalf("spurious error from (0, nil) reads: %v", r.Err())
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"a":2}` {
		t.Fatalf("got %q", got)
	}
}

// One byte per Read with io.EOF attached exactly at the value boundary.
func TestReaderOneBytePerReadEOFAtBoundary(t *testing.T) {
	payload := `{"a":1}` + "\n" + "42"
	var steps []scriptStep
	for i := 0; i < len(payload); i++ {
		step := scriptStep{data: []byte{payload[i]}}
		if i == len(payload)-1 {
			step.err = io.EOF
		}
		steps = append(steps, step)
	}
	r := newSizedReader(&scriptedReader{steps: steps}, 512)
	got := collectValues(r)
	if r.Err() != nil {
		t.Fatalf("unexpected error: %v", r.Err())
	}
	if len(got) != 2 || got[1] != "42" {
		t.Fatalf("got %q", got)
	}
}

type panicAfterReader struct {
	inner    io.Reader
	panicOn  int
	calls    int
	panicked bool
}

func (r *panicAfterReader) Read(p []byte) (int, error) {
	r.calls++
	if r.calls == r.panicOn && !r.panicked {
		r.panicked = true
		panic("probe: reader panic")
	}
	return r.inner.Read(p)
}

// A panicking source must propagate (not be swallowed) and must not corrupt
// reader state observed afterwards.
func TestReaderSourcePanicPropagates(t *testing.T) {
	src := &panicAfterReader{inner: strings.NewReader(`{"a":1}` + "\n" + `{"a":2}` + "\n"), panicOn: 2}
	r := newSizedReader(src, 512)
	if !r.Next() || string(r.Bytes()) != `{"a":1}` {
		t.Fatalf("first value: %q err=%v", r.Bytes(), r.Err())
	}
	if !r.Next() || string(r.Bytes()) != `{"a":2}` {
		t.Fatalf("second value: %q err=%v", r.Bytes(), r.Err())
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("source panic did not propagate through Next")
			}
		}()
		r.Next() // needs a refill; Read call #2 panics
		t.Fatal("Next returned instead of panicking")
	}()
	// After the panic the reader must still behave consistently: the retry
	// reaches EOF and the stream ends cleanly.
	if r.Next() {
		t.Fatalf("unexpected value after panic: %q", r.Bytes())
	}
	if r.Err() != nil {
		t.Fatalf("unexpected error after panic: %v", r.Err())
	}
}

// A value of exactly MaxValueBytes bytes is delivered regardless of whether
// io.EOF arrives attached to its final bytes: only values longer than the
// limit stop the stream, independent of framing.
func TestReaderMaxValueExactLimitFramingIndependence(t *testing.T) {
	val := `{"k":"` + strings.Repeat("a", 504) + `"}` // exactly 512 bytes
	if len(val) != 512 {
		t.Fatal("fixture size")
	}

	run := func(steps []scriptStep) ([]string, error) {
		r := newConfiguredReader(&scriptedReader{steps: steps}, 512, 512)
		got := collectValues(r)
		return got, r.Err()
	}
	// Framing A: data, then a separate (0, io.EOF).
	gotA, errA := run([]scriptStep{{data: []byte(val)}})
	// Framing B: data with io.EOF attached.
	gotB, errB := run([]scriptStep{{data: []byte(val), err: io.EOF}})

	if (errA == nil) != (errB == nil) || len(gotA) != len(gotB) {
		t.Errorf("outcome depends on framing:\n separate EOF: values=%d err=%v\n attached EOF: values=%d err=%v",
			len(gotA), errA, len(gotB), errB)
	}
}

// A configured value limit applies even when it is smaller than the initial
// buffer. Enforcement must not depend on whether the current value fits
// inside that buffer or requires growth; either way, an oversized value
// stops the stream with a limit error.
func TestReaderMaxValueEnforcedBelowBufferSize(t *testing.T) {
	val := `{"k":"` + strings.Repeat("a", 992) + `"}` // 1000 bytes
	r := newConfiguredReader(strings.NewReader(val+"\n"), 4096, 100)
	if r.Next() {
		t.Errorf("value of %d bytes delivered despite a %d byte limit", len(val), 100)
	} else if r.Err() == nil || !strings.Contains(r.Err().Error(), "limit") {
		t.Errorf("expected limit error, got %v", r.Err())
	}
}

// Discovering a clean end may compact the buffer after the last value.
// InputOffset must retain its absolute coordinate across that compaction,
// never exceed the total bytes consumed, and remain independent of buffer
// geometry.
func TestReaderInputOffsetAfterCleanEnd(t *testing.T) {
	val := `{"k":"` + strings.Repeat("a", 503) + `"}` // 511 bytes
	data := val + "\n"                                // 512 bytes: fills the 512-byte buffer exactly

	offsets := map[int]int64{}
	for _, size := range []int{512, 1024} {
		r := newSizedReader(strings.NewReader(data), size)
		if !r.Next() {
			t.Fatalf("size %d: %v", size, r.Err())
		}
		if off := r.InputOffset(); off != int64(len(val)) {
			t.Fatalf("size %d: offset after value = %d, want %d", size, off, len(val))
		}
		if r.Next() || r.Err() != nil {
			t.Fatalf("size %d: expected clean end, err=%v", size, r.Err())
		}
		offsets[size] = r.InputOffset()
	}
	for size, off := range offsets {
		if off > int64(len(data)) {
			t.Errorf("buffer %d: InputOffset after clean end = %d, exceeds total input length %d", size, off, len(data))
		}
	}
	if offsets[512] != offsets[1024] {
		t.Errorf("InputOffset after clean end depends on buffer size: %d vs %d", offsets[512], offsets[1024])
	}
}

// --- Attack surface 3: DecodeNext / DecodeFrom ------------------------------

type streamContractRecord struct {
	A int `json:"a"`
}

// A decode error mid-stream (valid JSON, wrong shape for the decoder) must
// terminate the stream with a positioned error — no silent skip — and leave
// Err sticky for both DecodeNext and Next.
func TestDecodeNextTypeMismatchMidStream(t *testing.T) {
	dec, err := CompileDecoder[streamContractRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := `{"a":1}` + "\n" + `"nope"` + "\n" + `{"a":3}` + "\n"
	r := newSizedReader(strings.NewReader(data), 512)

	var v streamContractRecord
	if err := DecodeFrom(r, dec, &v); err == nil {
		t.Fatal("DecodeFrom before Next must error")
	}
	if !DecodeNext(r, dec, &v) || v.A != 1 {
		t.Fatalf("first value: %+v err=%v", v, r.Err())
	}
	if off := r.InputOffset(); off != 7 {
		t.Fatalf("offset after first value = %d", off)
	}
	if DecodeNext(r, dec, &v) {
		t.Fatal("mismatched value must not decode")
	}
	if r.Err() == nil || !strings.Contains(r.Err().Error(), "offset 8") {
		t.Fatalf("expected error positioned at offset 8, got %v", r.Err())
	}
	if DecodeNext(r, dec, &v) || r.Next() {
		t.Fatal("stream must be terminally errored, not silently skipping")
	}
	if off := r.InputOffset(); off != 7 {
		// Not asserted as a failure: after an error there is no current
		// value, so the doc makes no promise. Recorded for visibility.
		t.Logf("note: InputOffset after mid-stream decode error = %d (end of last good value is 7)", off)
	}
}

// The positioned error message must survive buffer compaction (r.consumed
// accounting) when the offending value is larger than the remaining buffer.
func TestDecodeNextErrorOffsetAfterCompaction(t *testing.T) {
	dec, err := CompileDecoder[streamContractRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bad := `{"a":"` + strings.Repeat("x", 600) + `"}` // string where int expected, forces compaction+growth in a 512 buffer
	data := `{"a":1}` + "\n" + bad + "\n"
	r := newSizedReader(strings.NewReader(data), 512)
	var v streamContractRecord
	if !DecodeNext(r, dec, &v) || v.A != 1 {
		t.Fatalf("first value: %+v err=%v", v, r.Err())
	}
	if DecodeNext(r, dec, &v) {
		t.Fatal("mismatched value must not decode")
	}
	if r.Err() == nil || !strings.Contains(r.Err().Error(), "offset 8") {
		t.Fatalf("expected error positioned at offset 8 after compaction, got %v", r.Err())
	}
}

// DecodeNext and Next may be alternated freely on one Reader.
func TestAlternatingNextAndDecodeNext(t *testing.T) {
	dec, err := CompileDecoder[streamContractRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&data, "{\"a\":%d}\n", i)
	}
	r := newSizedReader(&chunkReader{data: data.Bytes(), chunk: 3}, 512)
	for i := 0; i < 40; i++ {
		if i%2 == 0 {
			var v streamContractRecord
			if !DecodeNext(r, dec, &v) {
				t.Fatalf("row %d: %v", i, r.Err())
			}
			if v.A != i {
				t.Fatalf("row %d: got %+v", i, v)
			}
			if want := fmt.Sprintf(`{"a":%d}`, i); string(r.Bytes()) != want {
				t.Fatalf("row %d: Bytes %q want %q", i, r.Bytes(), want)
			}
		} else {
			if !r.Next() {
				t.Fatalf("row %d: %v", i, r.Err())
			}
			var v streamContractRecord
			if err := DecodeFrom(r, dec, &v); err != nil || v.A != i {
				t.Fatalf("row %d: %+v err=%v", i, v, err)
			}
		}
	}
	if r.Next() || r.Err() != nil {
		t.Fatalf("expected clean end, err=%v", r.Err())
	}
}

// DecodeNext on a value beyond MaxValueBytes must stop with the limit error
// rather than growing without bound or spinning.
func TestDecodeNextOverMaxValue(t *testing.T) {
	dec, err := CompileDecoder[streamContractRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	big := `{"a":` + strings.Repeat("1", 2000) + `}`
	r := newConfiguredReader(strings.NewReader(big+"\n"), 512, 512)
	var v streamContractRecord
	if DecodeNext(r, dec, &v) {
		t.Fatal("oversized value must not decode")
	}
	if r.Err() == nil || !strings.Contains(r.Err().Error(), "limit") {
		t.Fatalf("expected limit error, got %v", r.Err())
	}
}

// --- Attack surface 2 and 5: Writer state machine and emitters -----------

// Flush moves buffered bytes to the sink without framing values: a second
// top-level value still requires Newline, and the guard error must not
// advertise Flush as an escape hatch.
