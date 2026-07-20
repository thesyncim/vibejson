package byteview

import (
	"bytes"
	"testing"
)

func TestCallerValidatedByteRange(t *testing.T) {
	src := []byte("abcdef")
	base := &src[0]
	if got := ByteAt(base, 3); got != 'd' {
		t.Fatalf("ByteAt(base, 3) = %q, want d", got)
	}
	view := SliceRange(base, 1, 5)
	if !bytes.Equal(view, []byte("bcde")) {
		t.Fatalf("SliceRange(base, 1, 5) = %q, want bcde", view)
	}
	src[2] = 'X'
	if string(view) != "bXde" {
		t.Fatalf("SliceRange copied its source: %q", view)
	}
}
