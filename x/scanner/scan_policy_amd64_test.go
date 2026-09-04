//go:build !go1.28 && goexperiment.simd && amd64

package scanner

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAMD64ScannerFallbackCoversPublicVectorEntries(t *testing.T) {
	originalLevel := scanAMD64Level
	scanAMD64Level = scanLevelScalar
	defer func() { scanAMD64Level = originalLevel }()
	if scanAVX2Available() {
		t.Skip("GOAMD64=v3 and later require AVX2 and use direct dispatch")
	}
	for _, text := range []string{
		"", strings.Repeat("a", 128), strings.Repeat("日本語", 32),
		strings.Repeat("a", 65) + "\u2028", strings.Repeat("a", 65) + "\u2029",
		strings.Repeat("a", 65) + "\xff", strings.Repeat("a", 65) + "\xf0\x9f",
		strings.Repeat("a", 65) + "<>&\"\\",
	} {
		src := []byte(text)
		if got := ValidUTF8(src); got != utf8.Valid(src) {
			t.Fatalf("ValidUTF8(%q) = %v", src, got)
		}
		want := utf8.Valid(src) && !hasJSONLineSeparatorScalar(src, 0)
		if got := ValidUTF8NoLineSeparator(src); got != want {
			t.Fatalf("ValidUTF8NoLineSeparator(%q) = %v, want %v", src, got, want)
		}
		for _, html := range []bool{false, true} {
			dst := make([]byte, len(src))
			want := scanStringSpecialScalar(src, 0)
			got := CopyStringPrefix(dst, src)
			if html {
				want = scanEncodedHTMLSpecialScalar(src, 0)
				got = CopyHTMLStringPrefix(dst, src)
			}
			if got != want || !bytes.Equal(dst[:got], src[:want]) {
				t.Fatalf("copy prefix (html=%v) = %d, want %d", html, got, want)
			}
		}
	}
	// Declining the vector escape batch is observable: the scalar parser must
	// handle the run itself when this machine has no SIMD backend.
	src := []byte(strings.Repeat(`\u0061`, 32))
	if end, ok := ScanUnicodeEscapeRun(src, 0); end != 0 || !ok {
		t.Fatalf("scalar escape batch = %d, %v; want 0, true", end, ok)
	}
}

func TestAMD64ScannerSelectionRequiresProvenWidth(t *testing.T) {
	cases := []struct {
		name    string
		hasAVX2 bool
		want    uint8
	}{
		{"scalar", false, scanLevelScalar},
		{"avx2", true, scanLevelAVX2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectAMD64ScannerLevel(tc.hasAVX2); got != tc.want {
				t.Fatalf("selectAMD64ScannerLevel(%v) = %d, want %d", tc.hasAVX2, got, tc.want)
			}
		})
	}
}

func TestAMD64ScannerCrossoverMatchesScalar(t *testing.T) {
	originalLevel := scanAMD64Level
	defer func() { scanAMD64Level = originalLevel }()
	levels := []uint8{scanLevelScalar}
	if originalLevel == scanLevelAVX2 {
		levels = append(levels, scanLevelAVX2)
	}
	lengths := []int{32, 33, 38, 39, 40, 47, 48, 55, 56, 64}
	positions := []int{0, 5, 15, 16, 23, 24, 31, 32, 39, 40, 47, 48, 55, 63}
	for _, level := range levels {
		scanAMD64Level = level
		for _, length := range lengths {
			clean := longScanCase(length, -1, 0)
			for start := 0; start <= length; start++ {
				if got, want := scanStringSpecial(clean, start), scanStringSpecialScalar(clean, start); got != want {
					t.Fatalf("level=%d clean length=%d start=%d: got %d, want %d", level, length, start, got, want)
				}
			}
			for _, position := range append(positions, length-1) {
				if position >= length {
					continue
				}
				for _, special := range []byte{'"', '\\', 0x1f, 0x80} {
					src := longScanCase(length, position, special)
					for start := 0; start <= length; start++ {
						if got, want := scanStringSpecial(src, start), scanStringSpecialScalar(src, start); got != want {
							t.Fatalf("level=%d length=%d position=%d special=%#02x start=%d: got %d, want %d", level, length, position, special, start, got, want)
						}
					}
				}
			}
		}
	}
}
