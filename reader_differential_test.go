package slopjson

import (
	"bytes"
	"encoding/json"
	"io"
	"math/rand"
	"testing"
)

// stdlibStreamValues splits concatenated/whitespace-separated JSON with the
// standard library, returning each value compacted for comparison.
func stdlibStreamValues(data []byte) ([][]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var out [][]byte
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if err == io.EOF {
			return out, true
		}
		if err != nil {
			return out, false
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			return out, false
		}
		out = append(out, append([]byte(nil), buf.Bytes()...))
	}
}

func compact(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("compact %.60q: %v", b, err)
	}
	return append([]byte(nil), buf.Bytes()...)
}

// TestStreamOracle feeds valid concatenated JSON streams through the Reader
// (both Next and DecodeNext, at multiple buffer sizes and chunk framings) and
// checks the value sequence matches encoding/json's json.Decoder.
func TestStreamOracle(t *testing.T) {
	r := rand.New(rand.NewSource(0x57DEA))
	valueGens := []func(*rand.Rand) string{
		func(r *rand.Rand) string { return "123" },
		func(r *rand.Rand) string { return "-4.5e10" },
		func(r *rand.Rand) string { return "true" },
		func(r *rand.Rand) string { return "false" },
		func(r *rand.Rand) string { return "null" },
		func(r *rand.Rand) string { return `"a string with \" and \\ and \n"` },
		func(r *rand.Rand) string { return `"unicode 𝄞 and é"` },
		func(r *rand.Rand) string { return `{"k":1,"nested":{"a":[1,2,3]},"s":"x"}` },
		func(r *rand.Rand) string { return `[1,"two",{"three":3},[4,[5,[6]]]]` },
		func(r *rand.Rand) string { return `{}` },
		func(r *rand.Rand) string { return `[]` },
		func(r *rand.Rand) string { return `""` },
		func(r *rand.Rand) string {
			// a long string to exceed small buffers and span refills
			b := make([]byte, 200+r.Intn(4000))
			for i := range b {
				b[i] = byte('a' + r.Intn(26))
			}
			return `"` + string(b) + `"`
		},
	}
	// Non-empty separators only: two directly-concatenated bare scalars would
	// lex as a single token (e.g. "-4.5e10"+"123"), which is a value-semantics
	// question, not a framing one. Non-scalar concatenation is covered by
	// FuzzStreamReaderChunkEquivalence.
	seps := []string{" ", "\n", "\t", "\r\n", "  ", " \n "}

	for iter := 0; iter < testIterations(4_000, 100); iter++ {
		var sb bytes.Buffer
		count := 1 + r.Intn(8)
		for i := 0; i < count; i++ {
			if i > 0 {
				sb.WriteString(seps[r.Intn(len(seps))])
			}
			g := valueGens[r.Intn(len(valueGens))]
			s := g(r)
			// Two adjacent bare scalars need a separator to remain distinct.
			sb.WriteString(s)
			// A bare number/literal followed directly by another must be separated.
		}
		data := sb.Bytes()

		want, wantOK := stdlibStreamValues(data)
		if !wantOK {
			continue // ambiguous concatenation (e.g. "12" "34"); skip
		}

		for _, size := range []int{512, 513, 1024, 4096} {
			// Next path.
			checkReaderSeq(t, data, size, want, false)
			// DecodeNext path.
			checkReaderSeq(t, data, size, want, true)
		}
	}
}

var anyStreamDecoder = func() Decoder[any] {
	d, err := CompileDecoder[any](DecoderOptions{})
	if err != nil {
		panic(err)
	}
	return d
}()

func checkReaderSeq(t *testing.T, data []byte, size int, want [][]byte, useDecode bool) {
	t.Helper()
	reader := newSizedReader(&tornReader{data: append([]byte(nil), data...), state: uint64(size)*2654435761 | 1}, size)
	var got [][]byte
	if useDecode {
		for {
			var v any
			if !DecodeNext(reader, anyStreamDecoder, &v) {
				break
			}
			got = append(got, compact(t, reader.Bytes()))
		}
	} else {
		for reader.Next() {
			got = append(got, compact(t, reader.Bytes()))
		}
	}
	if reader.Err() != nil {
		t.Fatalf("reader error on valid stream (size=%d decode=%v): %v\ndata=%.200q", size, useDecode, reader.Err(), data)
	}
	if len(got) != len(want) {
		t.Fatalf("value count mismatch (size=%d decode=%v): got %d want %d\ndata=%.200q", size, useDecode, len(got), len(want), data)
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("value %d mismatch (size=%d decode=%v): got %.80q want %.80q", i, size, useDecode, got[i], want[i])
		}
	}
}
