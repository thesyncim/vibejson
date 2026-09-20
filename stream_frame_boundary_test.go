package vibejson

import (
	"bytes"
	"io"
	"testing"
)

func exactCapValue(v []byte) []byte {
	b := make([]byte, len(v))
	copy(b, v)
	return b[:len(b):len(b)]
}

func TestValueFrameNoOverreadAtCapBoundary(t *testing.T) {
	for _, base := range frameCorpus() {
		if len(base) == 0 {
			continue
		}
		for n := 1; n <= len(base); n++ {
			window := exactCapValue(base[:n])

			var fast ValueFrame
			var ref scalarFrame
			fast.Init(window[0])
			ref.init(window[0])
			fastDone := fast.Scan(window, 0, len(window))
			refDone := ref.scan(window, 0, len(window))
			if fastDone != refDone || fast.Framed != ref.framed {
				t.Fatalf("divergence on %.40q (n=%d, cap==len): simd(done=%v,framed=%d) scalar(done=%v,framed=%d)",
					base, n, fastDone, fast.Framed, refDone, ref.framed)
			}
		}
	}
}

type capBoundedReader struct {
	chunks [][]byte
	pos    int
}

func (c *capBoundedReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.pos])
	c.pos++
	if c.pos >= len(c.chunks) {
		return n, io.EOF
	}
	return n, nil
}

func TestReaderNoOverreadValueEndsAtBufferEnd(t *testing.T) {
	values := [][]byte{
		[]byte(`"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`),
		[]byte(`{"k":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
		[]byte(`[1,2,3,"aaaaaaaaaaaaaaaaaaaaaaaaaaaa"]`),
		[]byte(`"` + string(bytes.Repeat([]byte("z"), 130)) + `"`),
		[]byte(`123456789.0123456789e+5`),
		[]byte(`true`),
	}
	stream := bytes.Join(values, []byte("\n"))

	chunks := make([][]byte, 0, len(stream))
	for i := 0; i < len(stream); i++ {
		chunks = append(chunks, exactCapValue(stream[i:i+1]))
	}

	r := newSizedReader(&capBoundedReader{chunks: chunks}, 512)
	got := 0
	for r.Next() {
		want := bytes.TrimSpace(values[got])
		if !bytes.Equal(r.Bytes(), want) {
			t.Fatalf("value %d: got %q want %q", got, r.Bytes(), want)
		}
		got++
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if got != len(values) {
		t.Fatalf("got %d values, want %d", got, len(values))
	}
}
