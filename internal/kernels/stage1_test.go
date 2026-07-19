//go:build go1.27 && !go1.28 && goexperiment.simd && (arm64 || amd64)

package kernels

import "testing"

func stage1RefMasks(block *[64]byte) Stage1Masks {
	var m Stage1Masks
	for i, c := range block {
		bit := uint64(1) << i
		switch c {
		case ' ', '\t', '\n', '\r':
			m.Whitespace |= bit
		case '{', '}', '[', ']', ':', ',':
			m.Structural |= bit
		case '"':
			m.Quote |= bit
		case '\\':
			m.Backslash |= bit
		}
		if c < 0x20 {
			m.Control |= bit
		}
		if c >= 0x80 {
			m.NonASCII = true
		}
	}
	return m
}

func TestStage1BlockAllByteValues(t *testing.T) {
	// Every byte value at every lane position, one at a time, so any lane
	// permutation inside the kernel is fully exercised.
	for b := 0; b <= 0xff; b++ {
		var block [64]byte
		for i := range block {
			block[i] = 'a'
		}
		for at := 0; at < 64; at++ {
			block[at] = byte(b)
			var got Stage1Masks
			Stage1Block(&block, &got)
			want := stage1RefMasks(&block)
			if got != want {
				t.Fatalf("Stage1Block(byte=0x%02x at %d) = %+v, want %+v", b, at, got, want)
			}
			block[at] = 'a'
		}
	}
}

var stage1BenchSink Stage1Masks

func BenchmarkStage1Block(b *testing.B) {
	var block [64]byte
	copy(block[:], `{"key": "value", "n": 12345, "flag": true, "arr": [1,2,3]}   `)
	b.SetBytes(64)
	for i := 0; i < b.N; i++ {
		Stage1Block(&block, &stage1BenchSink)
	}
}

func TestStage1BlockRandom(t *testing.T) {
	state := uint64(0x9e3779b97f4a7c15)
	interesting := []byte{'"', '\\', '{', '}', '[', ']', ':', ',', ' ', '\t', '\n', '\r', 0x00, 0x1f, 0x7f, 0x80, 0xff, 'a', '0'}
	var block [64]byte
	for round := 0; round < 20000; round++ {
		for i := range block {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			block[i] = interesting[state%uint64(len(interesting))]
		}
		var got Stage1Masks
		Stage1Block(&block, &got)
		if want := stage1RefMasks(&block); got != want {
			t.Fatalf("Stage1Block(%q) = %+v, want %+v", block, got, want)
		}
	}
}
