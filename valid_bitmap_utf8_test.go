package simdjson

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/internal/kernels"
)

func TestSparseNonASCIIMask(t *testing.T) {
	rng := rand.New(rand.NewSource(0x55544638))
	for trial := 0; trial < 500; trial++ {
		blocks := 1 + rng.Intn(simdkernels.Stage1ChunkBlocks)
		src := make([]byte, blocks*64)
		if _, err := rng.Read(src); err != nil {
			t.Fatal(err)
		}
		var want uint32
		for block := 0; block < blocks; block++ {
			if rng.Intn(3) == 0 {
				src[block*64+rng.Intn(64)] |= 0x80
			}
			for _, c := range src[block*64 : (block+1)*64] {
				if c >= 0x80 {
					want |= 1 << block
					break
				}
			}
		}
		if got := sparseNonASCIIMask(unsafe.Pointer(unsafe.SliceData(src)), blocks); got != want {
			t.Fatalf("trial %d mask mismatch: got %032b want %032b", trial, got, want)
		}
	}
}

func TestBitmapUTF8RunReject(t *testing.T) {
	// A >64KiB whitespace-heavy document with multi-byte UTF-8 in its values.
	// In a SIMD build it is large and sparse enough to commit the bitmap
	// validation engine, so this exercises the engine's per-run UTF-8 check;
	// in a pure-Go build the same assertions cover the recursive validator.
	var b bytes.Buffer
	b.WriteString("{\n")
	for i := 0; i < 3000; i++ {
		b.WriteString(strings.Repeat(" ", 24))
		b.WriteString("\"k")
		for j := 0; j < 3; j++ {
			b.WriteByte(byte('a' + (i+j)%26))
		}
		b.WriteString("\": \"vé\",\n") // valid two-byte UTF-8 in values
	}
	b.WriteString(strings.Repeat(" ", 24))
	b.WriteString("\"end\": \"x\"\n}")
	good := b.Bytes()
	if len(good) < validBitmapMinBytes {
		t.Fatalf("doc too small: %d", len(good))
	}

	// Confirm this document actually commits the bitmap engine so its own
	// verdict is under test and not silently bypassed for the recursive path.
	if ok, decided := validBitmap(good); !decided || !ok {
		t.Fatalf("expected engine to accept and commit: ok=%v decided=%v", ok, decided)
	}

	// The public contract holds in every build: the valid document validates,
	// and each corruption of a multi-byte sequence is rejected.
	if err := validateOptions(good, Options{}); err != nil {
		t.Fatalf("Validate rejected valid document: %v", err)
	}
	cases := map[string]func([]byte) []byte{
		"lone continuation":  func(d []byte) []byte { d[bytes.IndexByte(d, 0xc3)+1] = 'x'; return d },
		"truncated lead":     func(d []byte) []byte { d[bytes.IndexByte(d, 0xc3)] = 0xe2; return d },
		"overlong":           func(d []byte) []byte { i := bytes.IndexByte(d, 0xc3); d[i] = 0xc0; d[i+1] = 0xaf; return d },
		"bad at last block":  func(d []byte) []byte { i := bytes.LastIndexByte(d, 0xc3); d[i+1] = 0x20; return d },
		"lead at slice tail": func(d []byte) []byte { i := bytes.LastIndexByte(d, 0xc3); d[i] = 0xf0; return d },
	}
	for name, mut := range cases {
		bad := mut(append([]byte(nil), good...))
		if validateOptions(bad, Options{}) == nil {
			t.Errorf("%s: Validate accepted invalid UTF-8", name)
		}
		if ok, decided := validBitmap(bad); decided && ok {
			t.Errorf("%s: engine accepted invalid UTF-8", name)
		}
	}
}
