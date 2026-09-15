//go:build !go1.28 && goexperiment.simd && (arm64 || amd64)

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
	return scanStringSpecialLong(src[:], 0)
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
		src := scanTestBytes(160, 80, byte(b))
		for _, start := range starts {
			checkScans(t, "string syntax", src, start, scanStringSyntaxScalar(src, start),
				scanCheck{"selected", scanStringSyntax}, scanCheck{"direct SIMD", scanStringSyntaxSIMD})
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

func TestSIMDUTF8MutatedBoundaries(t *testing.T) {
	// Move valid two-, three-, and four-byte sequences across every lane and
	// mutate every byte. Random bytes alone rarely reach the later blocks.
	for padding := 0; padding < 32; padding++ {
		src := make([]byte, padding, padding+128)
		for i := range src {
			src[i] = 'a'
		}
		src = append(src, "¢ࠀ퟿𐀀\U0010ffff-日本語-🙂-abcdefghijklmnopqrstuvwxyz"...)
		check := func(input []byte) {
			t.Helper()
			if got, want := validUTF8Fast(input), utf8.Valid(input); got != want {
				t.Fatalf("UTF-8 parity: padding=%d input=%x got=%v want=%v", padding, input, got, want)
			}
		}
		for end := 0; end <= len(src); end++ {
			check(src[:end])
		}
		for pos, original := range src {
			for value := 0; value < 256; value++ {
				src[pos] = byte(value)
				check(src)
			}
			src[pos] = original
		}
	}
}

func TestSIMDUTF8NoLineSeparatorBoundaries(t *testing.T) {
	for position := 0; position <= 96; position++ {
		for _, last := range []byte{0xa8, 0xa9} {
			src := scanTestBytes(128, -1, 0)
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
			checkScans(t, "string special", src, start, scanStringSpecialScalar(src, start),
				scanCheck{"selected", scanStringSpecial}, scanCheck{"long", scanStringSpecialLong})
		}
	}
}

func TestSIMDLongScanMatchesScalar(t *testing.T) {
	specials := []byte{'"', '\\', 0x1f, 0x80}
	positions := []int{0, 1, 15, 16, 17, 63, 64, 65, 127, 128, 129, 255, 256, 511, 512, 513, 700, 1023}
	starts := []int{0, 1, 7, 15, 16, 31, 64, 127, 128, 255, 511, 512}

	for _, pos := range positions {
		for _, special := range specials {
			src := scanTestBytes(1200, pos, special)
			for _, start := range starts {
				checkScans(t, "long string special", src, start, scanStringSpecialScalar(src, start),
					scanCheck{"long", scanStringSpecialLong}, scanCheck{"direct SIMD", scanStringSpecialSIMD})
			}
		}
	}

	src := scanTestBytes(1200, -1, 0)
	for _, start := range starts {
		checkScans(t, "long clean string special", src, start, scanStringSpecialScalar(src, start),
			scanCheck{"long", scanStringSpecialLong}, scanCheck{"direct SIMD", scanStringSpecialSIMD})
	}
}

func TestSIMDScanMatchesScalarAllByteValues(t *testing.T) {
	starts := []int{0, 1, 63, 64, 79, 80, 81}
	for b := 0; b <= 0xff; b++ {
		src := scanTestBytes(160, 80, byte(b))
		for _, start := range starts {
			checkScans(t, "string special", src, start, scanStringSpecialScalar(src, start),
				scanCheck{"selected", scanStringSpecial}, scanCheck{"direct SIMD", scanStringSpecialSIMD})
		}
	}
}

func TestSIMDEncodedHTMLScannersMatchScalar(t *testing.T) {
	starts := []int{0, 1, 15, 16, 31, 63, 64, 79, 80, 81, 159, 160}
	for b := 0; b <= 0xff; b++ {
		src := scanTestBytes(192, 80, byte(b))
		for _, start := range starts {
			checkScans(t, "HTML special", src, start, scanEncodedHTMLSpecialScalar(src, start),
				scanCheck{"selected", scanEncodedHTMLSpecialFast}, scanCheck{"direct SIMD", scanEncodedHTMLSpecialSIMD})
			checkScans(t, "HTML syntax", src, start, scanEncodedHTMLSyntaxScalar(src, start),
				scanCheck{"selected", scanEncodedHTMLSyntaxFast}, scanCheck{"direct SIMD", scanEncodedHTMLSyntaxSIMD})
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
				checkPrefixCopy(t, "CopyStringPrefix", CopyStringPrefix, dst, src, len(src))
			}
		}
	}

	specials := []byte{'"', '\\', 0, 0x1f, 0x80, 0xff}
	for _, special := range specials {
		for at := 0; at < 96; at++ {
			src := scanTestBytes(96, at, special)
			checkPrefixCopy(t, "CopyStringPrefix", CopyStringPrefix, make([]byte, len(src)), src, at)
		}
	}
}

func TestSIMDCopyHTMLStringPrefix(t *testing.T) {
	src := scanTestBytes(257, -1, 0)
	dst := make([]byte, len(src))
	checkPrefixCopy(t, "CopyHTMLStringPrefix", CopyHTMLStringPrefix, dst, src, len(src))
	for _, special := range []byte{'"', '\\', '<', '>', '&', 0, 0x1f, 0x80, 0xff} {
		for at := 0; at < 96; at++ {
			dirty := scanTestBytes(96, at, special)
			checkPrefixCopy(t, "CopyHTMLStringPrefix", CopyHTMLStringPrefix, make([]byte, len(dirty)), dirty, at)
		}
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
				checkScans(t, "bounded HTML special", src, start, scanEncodedHTMLSpecialScalar(src, start),
					scanCheck{"selected", scanEncodedHTMLSpecialFast})
				checkScans(t, "bounded HTML syntax", src, start, scanEncodedHTMLSyntaxScalar(src, start),
					scanCheck{"selected", scanEncodedHTMLSyntaxFast})
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
					checkScans(t, "bounded string special", src, start, scanStringSpecialScalar(src, start),
						scanCheck{"selected", scanStringSpecial}, scanCheck{"direct SIMD", scanStringSpecialSIMD})
					checkScans(t, "bounded string syntax", src, start, scanStringSyntaxScalar(src, start),
						scanCheck{"selected", scanStringSyntax}, scanCheck{"direct SIMD", scanStringSyntaxSIMD})
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
		checkScans(t, "string special", src, start, scanStringSpecialScalar(src, start),
			scanCheck{"selected", scanStringSpecial}, scanCheck{"long", scanStringSpecialLong}, scanCheck{"direct SIMD", scanStringSpecialSIMD})
		checkScans(t, "string syntax", src, start, scanStringSyntaxScalar(src, start),
			scanCheck{"selected", scanStringSyntax}, scanCheck{"direct SIMD", scanStringSyntaxSIMD})
		checkScans(t, "HTML special", src, start, scanEncodedHTMLSpecialScalar(src, start),
			scanCheck{"selected", scanEncodedHTMLSpecialFast}, scanCheck{"direct SIMD", scanEncodedHTMLSpecialSIMD})
		checkScans(t, "HTML syntax", src, start, scanEncodedHTMLSyntaxScalar(src, start),
			scanCheck{"selected", scanEncodedHTMLSyntaxFast}, scanCheck{"direct SIMD", scanEncodedHTMLSyntaxSIMD})

		if wantValid := utf8.Valid(src); validUTF8Fast(src) != wantValid {
			t.Fatalf("validUTF8Fast(%x) != %v", src, wantValid)
		}

		dst := make([]byte, len(src))
		checkPrefixCopy(t, "CopyStringPrefix", CopyStringPrefix, dst, src, scanStringSpecialScalar(src, 0))
		checkPrefixCopy(t, "CopyHTMLStringPrefix", CopyHTMLStringPrefix, dst, src, scanEncodedHTMLSpecialScalar(src, 0))
	})
}

func BenchmarkStringScannerASCII(b *testing.B) {
	lengths := []int{8, 15, 16, 24, 31, 32, 48, 63, 64, 96, 127, 128, 192, 255, 256, 384, 511, 512, 768, 1024}
	for _, n := range lengths {
		src := scanTestBytes(n, -1, 0)
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
		src := scanTestBytes(n, n-1, '"')
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
		src := scanTestBytes(n, -1, 0)
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
		src := scanTestBytes(n, -1, 0)
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
