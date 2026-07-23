package slopjson

import (
	"bytes"
	"io"
	"testing"
)

// exactCapValue copies v into a freshly allocated slice whose capacity equals
// its length, so any read past len(slice) lands outside the allocation. When
// the SIMD string-body scanner over-reads past the buffered extent n, that
// out-of-bounds access is on unallocated memory here rather than on slack
// capacity that would silently mask it. Running this under -race, or simply
// often enough that the trailing bytes fall on a guard page, surfaces the read.
func exactCapValue(v []byte) []byte {
	b := make([]byte, len(v))
	copy(b, v)
	return b[:len(b):len(b)]
}

// TestValueFrameNoOverreadAtCapBoundary drives the framer over slices whose
// capacity equals the buffered length at every prefix, the layout that lets an
// over-read escape the allocation. It also confirms the SIMD framer agrees with
// the scalar reference on the exact-capacity slice, so a scanner that quietly
// read one byte past n would either fault or diverge here.
func TestValueFrameNoOverreadAtCapBoundary(t *testing.T) {
	for _, base := range frameCorpus() {
		if len(base) == 0 {
			continue
		}
		for n := 1; n <= len(base); n++ {
			// The window handed to the framer is exactly the first n bytes,
			// with no slack capacity behind it: reading base[n] is out of
			// bounds of this allocation.
			window := exactCapValue(base[:n])

			var fast valueFrame
			var ref scalarFrame
			fast.init(window[0])
			ref.init(window[0])
			fastDone := fast.scan(window, 0, len(window))
			refDone := ref.scan(window, 0, len(window))
			if fastDone != refDone || fast.framed != ref.framed {
				t.Fatalf("divergence on %.40q (n=%d, cap==len): simd(done=%v,framed=%d) scalar(done=%v,framed=%d)",
					base, n, fastDone, fast.framed, refDone, ref.framed)
			}
		}
	}
}

// capBoundedReader hands out r.buf-style windows: every Read fills a slice
// whose backing array ends exactly where the returned data ends, so the
// Reader's internal buffer never has slack behind the last delivered byte.
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

// TestReaderNoOverreadValueEndsAtBufferEnd exercises the full Reader with values
// that repeatedly land the value end exactly at r.end while the buffer is being
// refilled a byte at a time. A value whose closing quote or bracket sits at the
// buffer edge is the worst case for a string-body scanner that reads a vector
// starting near the edge; the scan is bounded by r.end, so this must never read
// unread or unallocated tail bytes. Verified against the delivered values.
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

	// Reveal the stream in one-byte chunks so every value boundary is reached
	// with r.end sitting on the last byte of the value at some refill step.
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
