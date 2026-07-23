package slopjson

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

// tornReader yields the source in deterministic pseudo-random chunks,
// including single bytes and reads that return data together with io.EOF —
// every framing an io.Reader is allowed to produce.
type tornReader struct {
	data  []byte
	state uint64
}

func (r *tornReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	r.state ^= r.state << 13
	r.state ^= r.state >> 7
	r.state ^= r.state << 17
	n := 1 + int(r.state%97)
	if n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	if len(r.data) == 0 && r.state&1 == 0 {
		return n, io.EOF
	}
	return n, nil
}

type streamResult struct {
	values [][]byte
	err    error
	offset int64
}

func collectStream(t *testing.T, in io.Reader) streamResult {
	t.Helper()
	reader := newSizedReader(in, 64)
	var result streamResult
	for reader.Next() {
		value := reader.Bytes()
		if !strictJSONValid(value) {
			t.Fatalf("Reader produced a value that is not strict JSON: %.120q", value)
		}
		result.values = append(result.values, bytes.Clone(value))
	}
	result.err = reader.Err()
	result.offset = reader.InputOffset()
	return result
}

func compareStreamResults(t *testing.T, name string, whole, fragmented streamResult) {
	t.Helper()
	if (whole.err == nil) != (fragmented.err == nil) {
		t.Fatalf("%s error status depends on framing: whole %v, fragmented %v", name, whole.err, fragmented.err)
	}
	if whole.err != nil && whole.err.Error() != fragmented.err.Error() {
		t.Fatalf("%s error depends on framing: whole %q, fragmented %q", name, whole.err, fragmented.err)
	}
	if len(whole.values) != len(fragmented.values) {
		t.Fatalf("%s value count depends on framing: whole %d, fragmented %d", name, len(whole.values), len(fragmented.values))
	}
	for i := range whole.values {
		if !bytes.Equal(whole.values[i], fragmented.values[i]) {
			t.Fatalf("%s value %d depends on framing: %.80q vs %.80q", name, i, whole.values[i], fragmented.values[i])
		}
	}
	// InputOffset is specified only through the end of the current value.
	if whole.err == nil && whole.offset != fragmented.offset {
		t.Fatalf("%s input offset depends on framing: whole %d, fragmented %d", name, whole.offset, fragmented.offset)
	}
}

// adversarialStreamCorpus builds streaming inputs engineered to stress the
// resumable framer at chunk boundaries: dense escape runs, escaped quotes and
// backslashes, huge string bodies, deeply nested containers, and brackets that
// live inside strings so only correct in-string tracking frames them.
func adversarialStreamCorpus() [][]byte {
	bigStr := append(append([]byte{'"'}, bytes.Repeat([]byte("x"), 5000)...), '"')
	escRun := append(append([]byte{'"'}, bytes.Repeat([]byte(`\\`), 1000)...), '"')
	escQuotes := append(append([]byte{'"'}, bytes.Repeat([]byte(`a\"b`), 500)...), '"')
	deep := append(bytes.Repeat([]byte("["), 400), bytes.Repeat([]byte("]"), 400)...)
	deepObj := append(bytes.Repeat([]byte(`{"k":`), 200), append([]byte("0"), bytes.Repeat([]byte("}"), 200)...)...)
	bracketsInStr := []byte(`{"a":"}]}]}]","b":[{"c":"[[[["}]}`)
	unicodeEsc := append(append([]byte{'"'}, bytes.Repeat([]byte(`𝄞`), 100)...), '"')
	trailingBackslashes := append(append([]byte{'"'}, bytes.Repeat([]byte(`\\`), 999)...), '\\', '"', '"')
	corpus := [][]byte{
		bigStr,
		escRun,
		escQuotes,
		deep,
		deepObj,
		bracketsInStr,
		unicodeEsc,
		trailingBackslashes,
		[]byte(`"\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\"`),
		[]byte("[1,2,3]\n{\"a\":\"b\\\"c\"}\ntrue\nnull\n1.5e300\n"),
		bytes.Repeat([]byte(`{"k":"v"}`+"\n"), 50),
	}
	// Concatenations of several adversarial values back to back, no separators.
	var joined []byte
	for _, c := range corpus {
		joined = append(joined, c...)
	}
	corpus = append(corpus, joined)
	return corpus
}

// FuzzStreamReaderChunkEquivalence feeds the same bytes through the stream
// reader whole and torn into arbitrary chunks: the sequence of values, the
// error status, and the final input offset must not depend on framing. Inputs
// within the frame and cursor work budgets also hold SIMD framing to its scalar
// reference and Cursor walks to Parse's answers.
func FuzzStreamReaderChunkEquivalence(f *testing.F) {
	f.Add([]byte("{\"a\":1}\n[2,3]\ntrue\n"), uint64(1))
	f.Add([]byte(`1 2 3`), uint64(7))
	f.Add([]byte("\"split \\uD834\\uDD1E value\"\"back to back\""), uint64(9))
	f.Add([]byte("[1,2"), uint64(3))
	f.Add([]byte("  \r\n\t "), uint64(5))
	f.Add([]byte("nullnull"), uint64(11))
	f.Add(append(bytes.Repeat([]byte("{\"k\":\"vvvvvvvvvvvvvvvv\"}\n"), 40), "0.25\n"...), uint64(13))
	for _, data := range adversarialStreamCorpus() {
		f.Add(data, uint64(1))
		f.Add(data, uint64(3))
	}
	for _, stream := range cursorDifferentialStreams() {
		f.Add([]byte(stream), uint64(1))
	}
	for _, src := range frameCorpus() {
		f.Add(src, uint64(3))
	}
	f.Add([]byte("{\"a\":[1,2,{\"b\":null}],\"c\":\"d\"}\n[true,false]\n-12.5e3"), uint64(7))
	f.Fuzz(func(t *testing.T, data []byte, seed uint64) {
		if len(data) > 1<<16 {
			t.Skip()
		}
		checkValueFrameSIMDMatchesScalar(t, data, uint16(seed))
		whole := collectStream(t, bytes.NewReader(data))
		compareStreamResults(t, "variable chunks", whole,
			collectStream(t, &tornReader{data: data, state: seed | 1}))
		compareStreamResults(t, "fixed chunks", whole,
			collectStream(t, &fixedChunkReader{data: append([]byte(nil), data...), chunk: 1 + int(seed%17)}))

		if whole.err == nil {
			for i, value := range whole.values {
				if !json.Valid(value) {
					t.Fatalf("framed value %d not structurally valid JSON: %.80q", i, value)
				}
			}
		}
		checkValueCursorDifferential(t, data, seed)
	})
}
