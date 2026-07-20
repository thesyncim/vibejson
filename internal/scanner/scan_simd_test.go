//go:build go1.27 && !go1.28 && goexperiment.simd && (arm64 || amd64)

package scanner

import (
	"fmt"
	"runtime"
	"testing"
	"unicode/utf8"
)

var scanSink int
var copySink byte

//go:noinline
func scanStackBackedString() int {
	var src [128]byte
	for i := range src {
		src[i] = 'a'
	}
	return IndexStringSpecial(src[:], 0)
}

//go:noinline
func scanStackBackedStringLong() int {
	var src [128]byte
	for i := range src {
		src[i] = 'a'
	}
	return IndexStringSpecialLong(src[:], 0)
}

func TestSIMDScannerDispatch(t *testing.T) {
	info := Current()
	backend := info.Backend
	t.Logf("runtime SIMD scanner: backend=%s vector=%d min=%d", info.Backend, info.VectorBytes, info.MinBytes)
	if runtime.GOARCH == "arm64" && backend != "arm64-neon" {
		t.Fatalf("Current().Backend = %q on arm64, want arm64-neon", backend)
	}
	if backend == "scalar" {
		return
	}
	if info.VectorBytes < 16 || info.MinBytes < 16 {
		t.Fatalf("selected scanner has invalid runtime info: %+v", info)
	}
}

func TestSIMDScannerDispatchStaysOnStack(t *testing.T) {
	if allocs := testing.AllocsPerRun(1000, func() {
		scanSink = scanStackBackedString()
	}); allocs != 0 {
		t.Fatalf("stack-backed selected scanner allocs = %v, want 0", allocs)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		scanSink = scanStackBackedStringLong()
	}); allocs != 0 {
		t.Fatalf("stack-backed long scanner allocs = %v, want 0", allocs)
	}
}

func TestSIMDStringSyntaxMatchesScalarAllByteValues(t *testing.T) {
	starts := []int{0, 1, 31, 32, 63, 64, 79, 80, 81}
	for b := 0; b <= 0xff; b++ {
		src := longScanCase(160, 80, byte(b))
		for _, start := range starts {
			want := scanStringSyntaxScalar(src, start)
			got := scanStringSyntax(src, start)
			if got != want {
				t.Fatalf("scanStringSyntax(byte=0x%02x start=%d) = %d, want %d", b, start, got, want)
			}
			got = scanStringSyntaxSIMD(src, start)
			if got != want {
				t.Fatalf("scanStringSyntaxSIMD(byte=0x%02x start=%d) = %d, want %d", b, start, got, want)
			}
		}
	}
}

func TestSIMDUTF8MatchesStdlib(t *testing.T) {
	state := uint64(0x243f6a8885a308d3)
	storage := make([]byte, 32+512)
	for length := 0; length <= 512; length++ {
		for offset := 0; offset < 32; offset++ {
			src := storage[offset : offset+length]
			for i := range src {
				state ^= state << 13
				state ^= state >> 7
				state ^= state << 17
				src[i] = byte(state)
			}
			if got, want := validUTF8Fast(src), utf8.Valid(src); got != want {
				t.Fatalf("validUTF8Fast(length=%d offset=%d data=%x) = %v, want %v", length, offset, src, got, want)
			}
			wantClean := utf8.Valid(src) && !hasJSONLineSeparatorScalar(src, 0)
			if got := validUTF8NoLineSeparatorFast(src); got != wantClean {
				t.Fatalf("validUTF8NoLineSeparatorFast(length=%d offset=%d data=%x) = %v, want %v", length, offset, src, got, wantClean)
			}
		}
	}

	valid := []byte("ASCII-العربية-Հայերեն-বাংলা-日本語-🙂")
	for repeats := 1; repeats <= 32; repeats++ {
		src := make([]byte, 0, repeats*len(valid))
		for range repeats {
			src = append(src, valid...)
		}
		if !validUTF8Fast(src) {
			t.Fatalf("validUTF8Fast rejected %d-byte multilingual input", len(src))
		}
	}
}

func TestSIMDUTF8NoLineSeparatorBoundaries(t *testing.T) {
	for position := 0; position <= 96; position++ {
		for _, last := range []byte{0xa8, 0xa9} {
			src := longScanCase(128, -1, 0)
			src[position], src[position+1], src[position+2] = 0xe2, 0x80, last
			if validUTF8NoLineSeparatorFast(src) {
				t.Fatalf("accepted U+202%c at byte %d", '8'+rune(last-0xa8), position)
			}
		}
	}
	clean := []byte("ASCII-العربية-Հայերեն-বাংলা-日本語-🙂")
	if !validUTF8NoLineSeparatorFast(clean) {
		t.Fatal("rejected clean multilingual input")
	}
}

func TestSIMDJSONLineSeparatorMatchesScalar(t *testing.T) {
	state := uint64(0x13198a2e03707344)
	storage := make([]byte, 32+512)
	for length := 0; length <= 512; length++ {
		for offset := 0; offset < 32; offset++ {
			src := storage[offset : offset+length]
			for i := range src {
				state ^= state << 13
				state ^= state >> 7
				state ^= state << 17
				src[i] = byte(state)
			}
			if length >= 3 && (length+offset)%5 == 0 {
				at := (length*17 + offset) % (length - 2)
				src[at], src[at+1], src[at+2] = 0xe2, 0x80, 0xa8+byte((length+offset)&1)
			}
			for start := 0; start <= length; start++ {
				got := hasJSONLineSeparatorFast(src, start)
				want := hasJSONLineSeparatorScalar(src, start)
				if got != want {
					t.Fatalf("line separator(length=%d offset=%d start=%d data=%x) = %v, want %v", length, offset, start, src, got, want)
				}
			}
		}
	}
}

func TestSIMDScanMatchesScalar(t *testing.T) {
	cases := [][]byte{
		[]byte(`plain ascii without anything special`),
		[]byte(`quote " here`),
		[]byte(`slash \ here`),
		[]byte("control \x1f here"),
		[]byte("non-ascii \xe3\x81\x93 here"),
		[]byte(`0123456789abcdef"`),
		[]byte(`0123456789abcdef0123456789abcdef\`),
	}
	for _, src := range cases {
		for start := 0; start <= len(src); start++ {
			got := scanStringSpecial(src, start)
			want := scanStringSpecialScalar(src, start)
			if got != want {
				t.Fatalf("scanStringSpecial(%q, %d) = %d, want %d", src, start, got, want)
			}
			got = scanStringSpecialLong(src, start)
			if got != want {
				t.Fatalf("scanStringSpecialLong(%q, %d) = %d, want %d", src, start, got, want)
			}
		}
	}
}

func TestSIMDLongScanMatchesScalar(t *testing.T) {
	specials := []byte{'"', '\\', 0x1f, 0x80}
	positions := []int{0, 1, 15, 16, 17, 63, 64, 65, 127, 128, 129, 255, 256, 511, 512, 513, 700, 1023}
	starts := []int{0, 1, 7, 15, 16, 31, 64, 127, 128, 255, 511, 512}

	for _, pos := range positions {
		for _, special := range specials {
			src := longScanCase(1200, pos, special)
			for _, start := range starts {
				want := scanStringSpecialScalar(src, start)
				got := scanStringSpecialLong(src, start)
				if got != want {
					t.Fatalf("scanStringSpecialLong(pos=%d special=0x%x start=%d) = %d, want %d", pos, special, start, got, want)
				}
				got = scanStringSpecialSIMD(src, start)
				if got != want {
					t.Fatalf("scanStringSpecialSIMD(pos=%d special=0x%x start=%d) = %d, want %d", pos, special, start, got, want)
				}
			}
		}
	}

	src := longScanCase(1200, -1, 0)
	for _, start := range starts {
		want := scanStringSpecialScalar(src, start)
		got := scanStringSpecialLong(src, start)
		if got != want {
			t.Fatalf("scanStringSpecialLong(no special start=%d) = %d, want %d", start, got, want)
		}
		got = scanStringSpecialSIMD(src, start)
		if got != want {
			t.Fatalf("scanStringSpecialSIMD(no special start=%d) = %d, want %d", start, got, want)
		}
	}
}

func TestSIMDScanMatchesScalarAllByteValues(t *testing.T) {
	starts := []int{0, 1, 63, 64, 79, 80, 81}
	for b := 0; b <= 0xff; b++ {
		src := longScanCase(160, 80, byte(b))
		for _, start := range starts {
			want := scanStringSpecialScalar(src, start)
			got := scanStringSpecial(src, start)
			if got != want {
				t.Fatalf("scanStringSpecial(byte=0x%02x start=%d) = %d, want %d", b, start, got, want)
			}
			got = scanStringSpecialSIMD(src, start)
			if got != want {
				t.Fatalf("scanStringSpecialSIMD(byte=0x%02x start=%d) = %d, want %d", b, start, got, want)
			}
		}
	}
}

func TestSIMDEncodedHTMLScannersMatchScalar(t *testing.T) {
	starts := []int{0, 1, 15, 16, 31, 63, 64, 79, 80, 81, 159, 160}
	for b := 0; b <= 0xff; b++ {
		src := longScanCase(192, 80, byte(b))
		for _, start := range starts {
			wantSpecial := scanEncodedHTMLSpecialScalar(src, start)
			if got := scanEncodedHTMLSpecialFast(src, start); got != wantSpecial {
				t.Fatalf("HTML special byte=0x%02x start=%d: got %d, want %d", b, start, got, wantSpecial)
			}
			if got := scanEncodedHTMLSpecialSIMD(src, start); got != wantSpecial {
				t.Fatalf("direct HTML special byte=0x%02x start=%d: got %d, want %d", b, start, got, wantSpecial)
			}

			wantSyntax := scanEncodedHTMLSyntaxScalar(src, start)
			if got := scanEncodedHTMLSyntaxFast(src, start); got != wantSyntax {
				t.Fatalf("HTML syntax byte=0x%02x start=%d: got %d, want %d", b, start, got, wantSyntax)
			}
			if got := scanEncodedHTMLSyntaxSIMD(src, start); got != wantSyntax {
				t.Fatalf("direct HTML syntax byte=0x%02x start=%d: got %d, want %d", b, start, got, wantSyntax)
			}
		}
	}
}

func TestSIMDCopyStringPrefix(t *testing.T) {
	for length := 16; length <= 512; length++ {
		for srcOffset := 0; srcOffset < 32; srcOffset++ {
			for dstOffset := 0; dstOffset < 32; dstOffset++ {
				srcStorage := make([]byte, srcOffset+length)
				dstStorage := make([]byte, dstOffset+length)
				src := srcStorage[srcOffset:]
				dst := dstStorage[dstOffset:]
				for i := range src {
					src[i] = byte('a' + i%26)
				}
				if got := CopyStringPrefix(dst, src); got != len(src) {
					t.Fatalf("CopyStringPrefix(length=%d srcOffset=%d dstOffset=%d) = %d", length, srcOffset, dstOffset, got)
				}
				if string(dst) != string(src) {
					t.Fatalf("CopyStringPrefix(length=%d srcOffset=%d dstOffset=%d) copied different bytes", length, srcOffset, dstOffset)
				}
			}
		}
	}

	specials := []byte{'"', '\\', 0, 0x1f, 0x80, 0xff}
	for _, special := range specials {
		for at := 0; at < 96; at++ {
			src := longScanCase(96, at, special)
			if got := CopyStringPrefix(make([]byte, len(src)), src); got != at {
				t.Fatalf("CopyStringPrefix(byte %#02x at %d) = %d", special, at, got)
			}
		}
	}
}

func TestSIMDCopyHTMLStringPrefix(t *testing.T) {
	src := longScanCase(257, -1, 0)
	dst := make([]byte, len(src))
	if CopyHTMLStringPrefix(dst, src) != len(src) || string(dst) != string(src) {
		t.Fatal("CopyHTMLStringPrefix rejected or changed clean ASCII")
	}
	for _, special := range []byte{'"', '\\', '<', '>', '&', 0, 0x1f, 0x80, 0xff} {
		for at := 0; at < 96; at++ {
			dirty := longScanCase(96, at, special)
			if got := CopyHTMLStringPrefix(make([]byte, len(dirty)), dirty); got != at {
				t.Fatalf("CopyHTMLStringPrefix(byte %#02x at %d) = %d", special, at, got)
			}
		}
	}
}

func TestSIMDCopyStringPrefixRejectsInvalidBuffers(t *testing.T) {
	clean := []byte("0123456789abcdef0123456789abcdef")
	if CopyStringPrefix(make([]byte, len(clean)-1), clean) != -1 {
		t.Fatal("CopyStringPrefix accepted a short destination")
	}
	if CopyStringPrefix(clean, clean) != -1 {
		t.Fatal("CopyStringPrefix accepted identical slices")
	}
	storage := make([]byte, len(clean)+8)
	copy(storage, clean)
	if CopyStringPrefix(storage[4:4+len(clean)], storage[:len(clean)]) != -1 {
		t.Fatal("CopyStringPrefix accepted overlapping slices")
	}
}

func TestSIMDEncodedHTMLScannersRespectBounds(t *testing.T) {
	state := uint64(0x9e3779b97f4a7c15)
	for alignment := 0; alignment < 32; alignment++ {
		for length := 0; length <= 256; length++ {
			backing := make([]byte, alignment+length+64)
			src := backing[alignment : alignment+length : alignment+length]
			for i := range src {
				state ^= state << 13
				state ^= state >> 7
				state ^= state << 17
				src[i] = byte(state)
			}
			for start := 0; start <= length; start++ {
				wantSpecial := scanEncodedHTMLSpecialScalar(src, start)
				if got := scanEncodedHTMLSpecialFast(src, start); got != wantSpecial {
					t.Fatalf("HTML special alignment=%d length=%d start=%d: got %d, want %d", alignment, length, start, got, wantSpecial)
				}
				wantSyntax := scanEncodedHTMLSyntaxScalar(src, start)
				if got := scanEncodedHTMLSyntaxFast(src, start); got != wantSyntax {
					t.Fatalf("HTML syntax alignment=%d length=%d start=%d: got %d, want %d", alignment, length, start, got, wantSyntax)
				}
			}
		}
	}
}

func TestSIMDScannersRespectSliceBoundsAndAlignment(t *testing.T) {
	for alignment := 0; alignment < 32; alignment++ {
		for length := 0; length <= 192; length++ {
			backing := make([]byte, alignment+length+64)
			for i := range backing {
				backing[i] = 'a'
			}
			src := backing[alignment : alignment+length : alignment+length]
			for i := alignment + length; i < len(backing); i++ {
				// A vector load past len(src) would observe this immediately.
				backing[i] = '"'
			}

			positions := [...]int{-1, 0, length / 2, length - 1}
			for _, position := range positions {
				if position >= 0 && position < length {
					src[position] = '\\'
				}
				for start := 0; start <= length; start++ {
					wantSpecial := scanStringSpecialScalar(src, start)
					if got := scanStringSpecial(src, start); got != wantSpecial {
						t.Fatalf("special alignment=%d length=%d position=%d start=%d: got %d, want %d", alignment, length, position, start, got, wantSpecial)
					}
					if got := scanStringSpecialSIMD(src, start); got != wantSpecial {
						t.Fatalf("direct special alignment=%d length=%d position=%d start=%d: got %d, want %d", alignment, length, position, start, got, wantSpecial)
					}

					wantSyntax := scanStringSyntaxScalar(src, start)
					if got := scanStringSyntax(src, start); got != wantSyntax {
						t.Fatalf("syntax alignment=%d length=%d position=%d start=%d: got %d, want %d", alignment, length, position, start, got, wantSyntax)
					}
					if got := scanStringSyntaxSIMD(src, start); got != wantSyntax {
						t.Fatalf("direct syntax alignment=%d length=%d position=%d start=%d: got %d, want %d", alignment, length, position, start, got, wantSyntax)
					}
				}
				if position >= 0 && position < length {
					src[position] = 'a'
				}
			}
		}
	}
}

func FuzzSIMDScannersMatchScalar(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		[]byte(`plain ascii`),
		[]byte(`0123456789abcdef"tail`),
		[]byte("0123456789abcdef\\tail"),
		[]byte("0123456789abcdef\x1ftail"),
		[]byte("0123456789abcdef\xe2\x82\xa1tail"),
	} {
		f.Add(seed, uint16(0))
	}
	f.Fuzz(func(t *testing.T, src []byte, startSeed uint16) {
		if len(src) > 1<<16 {
			t.Skip("input too large for scanner fuzz")
		}
		start := 0
		if len(src) != 0 {
			start = int(startSeed) % (len(src) + 1)
		}
		wantSpecial := scanStringSpecialScalar(src, start)
		if got := scanStringSpecial(src, start); got != wantSpecial {
			t.Fatalf("dispatched special scan = %d, scalar = %d", got, wantSpecial)
		}
		if got := scanStringSpecialLong(src, start); got != wantSpecial {
			t.Fatalf("long special scan = %d, scalar = %d", got, wantSpecial)
		}
		if got := scanStringSpecialSIMD(src, start); got != wantSpecial {
			t.Fatalf("direct SIMD special scan = %d, scalar = %d", got, wantSpecial)
		}

		wantSyntax := scanStringSyntaxScalar(src, start)
		if got := scanStringSyntax(src, start); got != wantSyntax {
			t.Fatalf("dispatched syntax scan = %d, scalar = %d", got, wantSyntax)
		}
		if got := scanStringSyntaxSIMD(src, start); got != wantSyntax {
			t.Fatalf("direct SIMD syntax scan = %d, scalar = %d", got, wantSyntax)
		}

		wantHTMLSpecial := scanEncodedHTMLSpecialScalar(src, start)
		if got := scanEncodedHTMLSpecialFast(src, start); got != wantHTMLSpecial {
			t.Fatalf("HTML special scan = %d, scalar = %d", got, wantHTMLSpecial)
		}
		if got := scanEncodedHTMLSpecialSIMD(src, start); got != wantHTMLSpecial {
			t.Fatalf("direct SIMD HTML special scan = %d, scalar = %d", got, wantHTMLSpecial)
		}
		wantHTMLSyntax := scanEncodedHTMLSyntaxScalar(src, start)
		if got := scanEncodedHTMLSyntaxFast(src, start); got != wantHTMLSyntax {
			t.Fatalf("HTML syntax scan = %d, scalar = %d", got, wantHTMLSyntax)
		}
		if got := scanEncodedHTMLSyntaxSIMD(src, start); got != wantHTMLSyntax {
			t.Fatalf("direct SIMD HTML syntax scan = %d, scalar = %d", got, wantHTMLSyntax)
		}

		dst := make([]byte, len(src))
		wantPrefix := scanStringSpecialScalar(src, 0)
		if got := CopyStringPrefix(dst, src); got != wantPrefix {
			t.Fatalf("string prefix = %d, scalar = %d", got, wantPrefix)
		} else if string(dst[:got]) != string(src[:got]) {
			t.Fatal("string prefix copied different bytes")
		}
		wantHTMLPrefix := scanEncodedHTMLSpecialScalar(src, 0)
		if got := CopyHTMLStringPrefix(dst, src); got != wantHTMLPrefix {
			t.Fatalf("HTML prefix = %d, scalar = %d", got, wantHTMLPrefix)
		} else if string(dst[:got]) != string(src[:got]) {
			t.Fatal("HTML prefix copied different bytes")
		}
	})
}

func longScanCase(n, specialAt int, special byte) []byte {
	src := make([]byte, n)
	for i := range src {
		src[i] = 'a'
	}
	if specialAt >= 0 {
		src[specialAt] = special
	}
	return src
}

func BenchmarkStringScannerASCII(b *testing.B) {
	lengths := []int{8, 15, 16, 24, 31, 32, 48, 63, 64, 96, 127, 128, 192, 255, 256, 384, 511, 512, 768, 1024}
	for _, n := range lengths {
		src := longScanCase(n, -1, 0)
		b.Run(fmt.Sprintf("scalar/%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanSink = scanStringSpecialScalar(src, 0)
			}
		})
		b.Run(fmt.Sprintf("dispatch/%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanSink = scanStringSpecial(src, 0)
			}
		})
		b.Run(fmt.Sprintf("runtime/%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanSink = scanStringSpecialRuntime(src, 0)
			}
		})
		b.Run(fmt.Sprintf("direct/%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanSink = scanStringSpecialSIMD(src, 0)
			}
		})
	}
}

func BenchmarkStringScannerQuoteAtEnd(b *testing.B) {
	lengths := []int{16, 32, 64, 128, 256, 512, 1024}
	for _, n := range lengths {
		src := longScanCase(n, n-1, '"')
		b.Run(fmt.Sprintf("scalar/%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanSink = scanStringSpecialScalar(src, 0)
			}
		})
		b.Run(fmt.Sprintf("dispatch/%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanSink = scanStringSpecial(src, 0)
			}
		})
		b.Run(fmt.Sprintf("direct/%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanSink = scanStringSpecialSIMD(src, 0)
			}
		})
	}
}

func BenchmarkEncodedHTMLScannerASCII(b *testing.B) {
	lengths := []int{16, 17, 24, 31, 32, 33, 47, 48, 63, 64, 95, 96, 127, 128, 256, 512, 1024}
	for _, n := range lengths {
		src := longScanCase(n, -1, 0)
		b.Run(fmt.Sprintf("scalar/%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanSink = scanEncodedHTMLSpecialScalar(src, 0)
			}
		})
		b.Run(fmt.Sprintf("dispatch/%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanSink = scanEncodedHTMLSpecialFast(src, 0)
			}
		})
		b.Run(fmt.Sprintf("direct/%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				scanSink = scanEncodedHTMLSpecialSIMD(src, 0)
			}
		})
	}
}

func BenchmarkCopyHTMLStringPrefixASCII(b *testing.B) {
	lengths := []int{1, 4, 8, 15, 16, 17, 24, 31, 32, 33, 47, 48, 63, 64, 95, 96, 127, 128, 192, 256, 384, 512, 768, 1024, 2048}
	for _, n := range lengths {
		src := longScanCase(n, -1, 0)
		dst := make([]byte, n)
		b.Run(fmt.Sprintf("separate/%d", n), func(b *testing.B) {
			for range b.N {
				if scanEncodedHTMLSpecialFast(src, 0) == len(src) {
					copy(dst, src)
				}
			}
			copySink = dst[n-1]
		})
		b.Run(fmt.Sprintf("fused/%d", n), func(b *testing.B) {
			for range b.N {
				copyHTMLStringPrefix(dst, src)
			}
			copySink = dst[n-1]
		})
	}
}

func BenchmarkValidUTF8NoLineSeparator(b *testing.B) {
	unit := []byte("json-ハンドラ-héllo-🙂-données-")
	for _, n := range []int{64, 512, 4096} {
		src := make([]byte, 0, n+64)
		for len(src) < n {
			src = append(src, unit...)
		}
		src = src[:n:n]
		for len(src) > 0 && src[len(src)-1]&0xc0 == 0x80 {
			src = src[:len(src)-1]
		}
		b.Run(fmt.Sprintf("generic/%d", len(src)), func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			for range b.N {
				if !validUTF8NoLineSeparatorGeneric(src) {
					b.Fatal("rejected clean input")
				}
			}
		})
		b.Run(fmt.Sprintf("runtime/%d", len(src)), func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			for range b.N {
				if !validUTF8NoLineSeparatorRuntime(src) {
					b.Fatal("rejected clean input")
				}
			}
		})
	}
}
