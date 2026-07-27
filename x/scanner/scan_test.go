package scanner

import (
	"encoding/binary"
	"math/bits"
	"testing"
	"unicode/utf8"
)

func scanEncodedHTMLSpecialReference(src []byte, start int) int {
	for i := start; i < len(src); i++ {
		c := src[i]
		if c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' || c < 0x20 || c >= 0x80 {
			return i
		}
	}
	return len(src)
}

func scanEncodedHTMLSyntaxReference(src []byte, start int) int {
	for i := start; i < len(src); i++ {
		c := src[i]
		if c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' || c < 0x20 {
			return i
		}
	}
	return len(src)
}

func hasJSONLineSeparatorReference(src []byte, start int) bool {
	for i := start; i+2 < len(src); i++ {
		if src[i] == 0xe2 && src[i+1] == 0x80 && (src[i+2] == 0xa8 || src[i+2] == 0xa9) {
			return true
		}
	}
	return false
}

func scanTestRandomByte(state *uint64) byte {
	*state ^= *state << 13
	*state ^= *state >> 7
	*state ^= *state << 17
	return byte(*state)
}

func TestEncodedHTMLScalarFoldingMatchesReference(t *testing.T) {
	const length = 24
	src := make([]byte, length)
	for value := 0; value <= 0xff; value++ {
		for position := range src {
			for i := range src {
				src[i] = 'a'
			}
			src[position] = byte(value)
			for start := 0; start <= len(src); start++ {
				if got, want := scanEncodedHTMLSpecialScalar(src, start), scanEncodedHTMLSpecialReference(src, start); got != want {
					t.Fatalf("special byte=%#02x position=%d start=%d: got %d, want %d", value, position, start, got, want)
				}
				if got, want := scanEncodedHTMLSyntaxScalar(src, start), scanEncodedHTMLSyntaxReference(src, start); got != want {
					t.Fatalf("syntax byte=%#02x position=%d start=%d: got %d, want %d", value, position, start, got, want)
				}
			}
		}
	}

	state := uint64(0x243f6a8885a308d3)
	for iteration := 0; iteration < 4096; iteration++ {
		length := int(scanTestRandomByte(&state)) % 97
		random := make([]byte, length)
		for i := range random {
			random[i] = scanTestRandomByte(&state)
		}
		start := int(scanTestRandomByte(&state)) % (length + 1)
		if got, want := scanEncodedHTMLSpecialScalar(random, start), scanEncodedHTMLSpecialReference(random, start); got != want {
			t.Fatalf("random special iteration=%d data=%x start=%d: got %d, want %d", iteration, random, start, got, want)
		}
		if got, want := scanEncodedHTMLSyntaxScalar(random, start), scanEncodedHTMLSyntaxReference(random, start); got != want {
			t.Fatalf("random syntax iteration=%d data=%x start=%d: got %d, want %d", iteration, random, start, got, want)
		}
	}
}

func TestHasJSONLineSeparatorScalarBoundaries(t *testing.T) {
	for length := 0; length <= 96; length++ {
		src := make([]byte, length)
		for i := range src {
			src[i] = 'a'
		}
		for start := 0; start <= length; start++ {
			if hasJSONLineSeparatorScalar(src, start) {
				t.Fatalf("clean length=%d start=%d reported a separator", length, start)
			}
		}
		for position := 0; position+2 < length; position++ {
			for _, last := range []byte{0xa8, 0xa9} {
				src[position], src[position+1], src[position+2] = 0xe2, 0x80, last
				for start := 0; start <= length; start++ {
					got := hasJSONLineSeparatorScalar(src, start)
					want := position >= start
					if got != want {
						t.Fatalf("length=%d position=%d last=%#02x start=%d: got %v, want %v", length, position, last, start, got, want)
					}
				}
				src[position], src[position+1], src[position+2] = 'a', 'a', 'a'
			}
		}
	}

	state := uint64(0x13198a2e03707344)
	for iteration := 0; iteration < 8192; iteration++ {
		length := int(scanTestRandomByte(&state))
		src := make([]byte, length)
		for i := range src {
			src[i] = scanTestRandomByte(&state)
		}
		if length >= 3 && iteration%2 == 0 {
			position := int(scanTestRandomByte(&state)) % (length - 2)
			src[position], src[position+1], src[position+2] = 0xe2, 0x80, 0xa8+byte((iteration/2)&1)
		}
		starts := [...]int{0, length / 2, length, int(scanTestRandomByte(&state)) % (length + 1)}
		for _, start := range starts {
			if got, want := hasJSONLineSeparatorScalar(src, start), hasJSONLineSeparatorReference(src, start); got != want {
				t.Fatalf("random iteration=%d data=%x start=%d: got %v, want %v", iteration, src, start, got, want)
			}
		}
	}
}

func TestCopyStringPrefixPublicContract(t *testing.T) {
	clean := []byte("0123456789abcdef0123456789abcdef")
	dst := make([]byte, len(clean))
	if got := CopyStringPrefix(dst, clean); got != len(clean) || string(dst) != string(clean) {
		t.Fatalf("CopyStringPrefix(clean) = %d or changed bytes", got)
	}
	if got := CopyStringPrefix(make([]byte, len(clean)-1), clean); got != -1 {
		t.Fatalf("CopyStringPrefix(short dst) = %d, want -1", got)
	}
	if got := CopyStringPrefix(clean, clean); got != -1 {
		t.Fatalf("CopyStringPrefix(identical slices) = %d, want -1", got)
	}
	storage := make([]byte, len(clean)+8)
	copy(storage, clean)
	if got := CopyStringPrefix(storage[4:4+len(clean)], storage[:len(clean)]); got != -1 {
		t.Fatalf("CopyStringPrefix(overlap) = %d, want -1", got)
	}
	for _, special := range []byte{'"', '\\', 0, 0x1f, 0x80, 0xff} {
		dirty := append([]byte(nil), clean...)
		at := len(dirty) / 2
		dirty[at] = special
		if got := CopyStringPrefix(dst, dirty); got != at {
			t.Fatalf("CopyStringPrefix(byte %#02x) = %d, want %d", special, got, at)
		}
		if string(dst[:at]) != string(dirty[:at]) {
			t.Fatalf("CopyStringPrefix(byte %#02x) changed clean prefix", special)
		}
	}
}

func TestCopyHTMLStringPrefixPublicContract(t *testing.T) {
	clean := []byte("0123456789abcdef0123456789abcdef")
	dst := make([]byte, len(clean))
	if got := CopyHTMLStringPrefix(dst, clean); got != len(clean) || string(dst) != string(clean) {
		t.Fatalf("CopyHTMLStringPrefix(clean) = %d or changed bytes", got)
	}
	for _, special := range []byte{'"', '\\', '<', '>', '&', 0, 0x1f, 0x80, 0xff} {
		dirty := append([]byte(nil), clean...)
		at := len(dirty) / 2
		dirty[at] = special
		if got := CopyHTMLStringPrefix(dst, dirty); got != at {
			t.Fatalf("CopyHTMLStringPrefix(byte %#02x) = %d, want %d", special, got, at)
		}
	}
}

func scanEncodedHTMLSpecialScalarUnfolded(src []byte, i int) int {
	for i+8 <= len(src) {
		x := binary.LittleEndian.Uint64(src[i:])
		m := stringSpecialMask(x) | byteEqMask(x, '<') | byteEqMask(x, '>') | byteEqMask(x, '&')
		if m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
		i += 8
	}
	return scanEncodedHTMLSpecialReference(src, i)
}

func scanEncodedHTMLSyntaxScalarUnfolded(src []byte, i int) int {
	for i+8 <= len(src) {
		x := binary.LittleEndian.Uint64(src[i:])
		m := stringSyntaxMask(x) | byteEqMask(x, '<') | byteEqMask(x, '>') | byteEqMask(x, '&')
		if m != 0 {
			return i + bits.TrailingZeros64(m)/8
		}
		i += 8
	}
	return scanEncodedHTMLSyntaxReference(src, i)
}

func hasJSONLineSeparatorScalarBytewise(src []byte, start int) bool {
	for i := start; i+2 < len(src); i++ {
		if src[i] == 0xe2 && src[i+1] == 0x80 && (src[i+2] == 0xa8 || src[i+2] == 0xa9) {
			return true
		}
	}
	return false
}

func filledScanBytes(length int) []byte {
	src := make([]byte, length)
	for i := range src {
		src[i] = 'a'
	}
	return src
}

var scalarScanSink int
var scalarScanBoolSink bool

func BenchmarkEncodedHTMLScalarFolding(b *testing.B) {
	src := filledScanBytes(1024)
	b.Run("special/unfolded", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for range b.N {
			scalarScanSink = scanEncodedHTMLSpecialScalarUnfolded(src, 0)
		}
	})
	b.Run("special/folded", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for range b.N {
			scalarScanSink = scanEncodedHTMLSpecialScalar(src, 0)
		}
	})
	b.Run("syntax/unfolded", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for range b.N {
			scalarScanSink = scanEncodedHTMLSyntaxScalarUnfolded(src, 0)
		}
	})
	b.Run("syntax/folded", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for range b.N {
			scalarScanSink = scanEncodedHTMLSyntaxScalar(src, 0)
		}
	})
}

func BenchmarkJSONLineSeparatorScalarCandidates(b *testing.B) {
	falseCandidates := filledScanBytes(4096)
	for i := 0; i+2 < len(falseCandidates); i += 17 {
		falseCandidates[i] = 0xe2
	}
	separatorAtEnd := append([]byte(nil), falseCandidates...)
	separatorAtEnd[len(separatorAtEnd)-3] = 0xe2
	separatorAtEnd[len(separatorAtEnd)-2] = 0x80
	separatorAtEnd[len(separatorAtEnd)-1] = 0xa8
	cases := []struct {
		name string
		src  []byte
	}{
		{name: "ascii/64", src: filledScanBytes(64)},
		{name: "ascii/4096", src: filledScanBytes(4096)},
		{name: "false-candidates/4096", src: falseCandidates},
		{name: "separator-at-end/4096", src: separatorAtEnd},
	}
	for _, test := range cases {
		b.Run(test.name+"/bytewise", func(b *testing.B) {
			b.SetBytes(int64(len(test.src)))
			b.ReportAllocs()
			for range b.N {
				scalarScanBoolSink = hasJSONLineSeparatorScalarBytewise(test.src, 0)
			}
		})
		b.Run(test.name+"/candidate", func(b *testing.B) {
			b.SetBytes(int64(len(test.src)))
			b.ReportAllocs()
			for range b.N {
				scalarScanBoolSink = hasJSONLineSeparatorScalar(test.src, 0)
			}
		})
	}
}

// TestValidUTF8TailBoundaries pins the padded-final-block handling of the
// vector validators: sequences that dangle at the true end of input, complete
// sequences that straddle the last block boundary, and U+2028/U+2029 landing
// on or across it must classify exactly like the scalar oracles.
func TestValidUTF8TailBoundaries(t *testing.T) {
	sequences := [][]byte{
		{0xc3, 0xa9},             // 2-byte
		{0xe2, 0x82, 0xac},       // 3-byte
		{0xf0, 0x9f, 0x99, 0x82}, // 4-byte
		{0xe2, 0x80, 0xa8},       // U+2028
		{0xe2, 0x80, 0xa9},       // U+2029
		{0xc3},                   // dangling lead
		{0xe2, 0x82},             // dangling 3-byte prefix
		{0xf0, 0x9f, 0x99},       // dangling 4-byte prefix
		{0xbf},                   // orphan continuation
		{0xed, 0xa0, 0x80},       // surrogate half
		{0xc0, 0xaf},             // overlong
		{0xf5, 0x80, 0x80, 0x80}, // above U+10FFFF
	}
	// Slide each sequence so it ends at, before, and after every block
	// boundary of a two-block input, including ends aligned exactly to
	// multiples of 16 where the padded tail block is all zeros.
	for _, sequence := range sequences {
		for total := 14; total <= 40; total++ {
			for end := len(sequence); end <= total; end++ {
				src := make([]byte, total)
				for i := range src {
					src[i] = 'a'
				}
				copy(src[end-len(sequence):], sequence)
				wantValid := utf8.Valid(src)
				if got := validUTF8Fast(src); got != wantValid {
					t.Fatalf("validUTF8Fast(len=%d end=%d seq=%x) = %v, want %v", total, end, sequence, got, wantValid)
				}
				wantClean := wantValid && !hasJSONLineSeparatorScalarBytewise(src, 0)
				if got := validUTF8NoLineSeparatorFast(src); got != wantClean {
					t.Fatalf("validUTF8NoLineSeparatorFast(len=%d end=%d seq=%x) = %v, want %v", total, end, sequence, got, wantClean)
				}
			}
		}
	}
}
