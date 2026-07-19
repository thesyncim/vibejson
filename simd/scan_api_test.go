package simd

import (
	"strings"
	"testing"
)

// scanUnicodeEscapeRunReference is an independent scalar model of
// scanUnicodeEscapeRun's contract, written from the documented behavior rather
// than the kernel: starting at i, consume complete groups of eight contiguous
// \uXXXX escapes (48 bytes each), and stop returning the current position
// before a partial group, before any byte that breaks the \uXXXX shape or hex
// alphabet, and before any escape whose first hex digit is d or D (the 0xDxxx
// surrogate range the scalar decoder must handle itself). ok is always true;
// rejection is signalled by not advancing.
func scanUnicodeEscapeRunReference(src []byte, i int) (int, bool) {
	if i < 0 {
		i = 0
	}
	if i > len(src) {
		i = len(src)
	}
	isHex := func(b byte) bool {
		return (b >= '0' && b <= '9') || (b|0x20 >= 'a' && b|0x20 <= 'f')
	}
	for i+48 <= len(src) {
		ok := true
		for e := 0; e < 8 && ok; e++ {
			off := i + e*6
			if src[off] != '\\' || src[off+1] != 'u' {
				ok = false
				break
			}
			if src[off+2]|0x20 == 'd' { // surrogate range: leave to the scalar path
				ok = false
				break
			}
			for d := 2; d < 6; d++ {
				if !isHex(src[off+d]) {
					ok = false
					break
				}
			}
		}
		if !ok {
			return i, true
		}
		i += 48
	}
	return i, true
}

func TestScannerAPINormalizesStart(t *testing.T) {
	src := []byte("plain<text\\\"\xe2\x80\xa8tail")
	starts := []int{-100, -1, 0, 5, len(src), len(src) + 1, len(src) + 100}
	for _, start := range starts {
		clamped := start
		if clamped < 0 {
			clamped = 0
		} else if clamped > len(src) {
			clamped = len(src)
		}

		if got, want := IndexStringSpecial(src, start), IndexStringSpecial(src, clamped); got != want {
			t.Errorf("IndexStringSpecial(start=%d) = %d, want %d", start, got, want)
		}
		if got, want := IndexStringSpecialLong(src, start), IndexStringSpecialLong(src, clamped); got != want {
			t.Errorf("IndexStringSpecialLong(start=%d) = %d, want %d", start, got, want)
		}
		if got, want := IndexStringSyntax(src, start), IndexStringSyntax(src, clamped); got != want {
			t.Errorf("IndexStringSyntax(start=%d) = %d, want %d", start, got, want)
		}
		if got, want := IndexHTMLStringSpecial(src, start), IndexHTMLStringSpecial(src, clamped); got != want {
			t.Errorf("IndexHTMLStringSpecial(start=%d) = %d, want %d", start, got, want)
		}
		if got, want := IndexHTMLStringSyntax(src, start), IndexHTMLStringSyntax(src, clamped); got != want {
			t.Errorf("IndexHTMLStringSyntax(start=%d) = %d, want %d", start, got, want)
		}
		if got, want := HasJSONLineSeparator(src, start), HasJSONLineSeparator(src, clamped); got != want {
			t.Errorf("HasJSONLineSeparator(start=%d) = %v, want %v", start, got, want)
		}

		gotNext, gotBad := ScanStringUnicodeRun(src, start)
		wantNext, wantBad := ScanStringUnicodeRun(src, clamped)
		if gotNext != wantNext || gotBad != wantBad {
			t.Errorf("ScanStringUnicodeRun(start=%d) = (%d, %d), want (%d, %d)", start, gotNext, gotBad, wantNext, wantBad)
		}
	}
}

func TestScanUnicodeEscapeRunNormalizesStart(t *testing.T) {
	src := []byte(`\u1234\u5678\u9abc\udef0\u1234\u5678\u9abc\uef01`)
	for _, start := range []int{-100, -1, 0, len(src), len(src) + 1, len(src) + 100} {
		clamped := start
		if clamped < 0 {
			clamped = 0
		} else if clamped > len(src) {
			clamped = len(src)
		}
		end, ok := ScanUnicodeEscapeRun(src, start)
		wantEnd, wantOK := ScanUnicodeEscapeRun(src, clamped)
		if end != wantEnd || ok != wantOK {
			t.Errorf("ScanUnicodeEscapeRun(start=%d) = (%d, %v), want (%d, %v)", start, end, ok, wantEnd, wantOK)
		}
	}
}

func TestScannerCopyAPISafety(t *testing.T) {
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
	if got := CopyHTMLStringPrefix(storage[4:4+len(clean)], storage[:len(clean)]); got != -1 {
		t.Fatalf("CopyHTMLStringPrefix(overlap) = %d, want -1", got)
	}
}

// TestScanUnicodeEscapeRunMatchesScalarReference checks the dispatched scanner
// against an independent scalar model over adversarial inputs: valid runs of
// several block sizes, breaks of each kind right after a full block, and every
// start position. Comparing against scanUnicodeEscapeRunReference, not the same
// kernel, is what makes the SIMD block logic actually tested.
func TestScanUnicodeEscapeRunMatchesScalarReference(t *testing.T) {
	// esc builds a literal 6-byte "\uHHHH" escape; code is 4 hex digits.
	esc := func(code string) string { return "\\u" + code }
	// A run of n non-surrogate escapes (first hex digit never d or D), enough
	// distinct codes to fill multiple full 48-byte blocks.
	codes := []string{"0041", "00e9", "1234", "abcd", "beef", "cafe", "0000", "ffff", "007f", "20ac"}
	run := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(esc(codes[i%len(codes)]))
		}
		return b.String()
	}
	block := run(8)          // exactly one 48-byte block
	surrogate := esc("d800") // first hex digit d: must stop the run

	// The vector kernel advances across full blocks; the scalar fallback never
	// advances and so cannot match a block-advancing reference. Probe the
	// compiled kernel on a clean block to decide which is built.
	if end, _ := ScanUnicodeEscapeRun([]byte(block), 0); end == 0 {
		t.Skip("scalar build: scanUnicodeEscapeRun does not advance across blocks")
	}

	inputs := []string{
		"",
		esc("0041"),                       // one escape, below a full block
		run(7),                            // seven escapes: never a full block
		block,                             // exactly one full block
		block + block,                     // two full blocks
		block + block + esc("00"),         // two blocks then a truncated escape
		block + surrogate + esc("0041"),   // block, then a lowercase surrogate-first escape
		block + esc("D800") + esc("0041"), // block, then an uppercase surrogate-first escape
		block + esc("12g4") + esc("0041"), // block, then an invalid hex digit
		block + "\\x1234" + esc("0041"),   // block, then a non-u escape
		block + "plain text after block",  // block, then non-escape bytes
		esc("123"),                        // truncated first escape
		"not an escape at all",
		run(32),                        // four full blocks, no break
		run(24) + surrogate + esc("1"), // three blocks then a surrogate
	}
	for _, in := range inputs {
		src := []byte(in)
		for start := -2; start <= len(src)+2; start++ {
			gotEnd, gotOK := ScanUnicodeEscapeRun(src, start)
			// ScanUnicodeEscapeRun clamps start; the reference clamps the same way.
			wantEnd, wantOK := scanUnicodeEscapeRunReference(src, start)
			if gotEnd != wantEnd || gotOK != wantOK {
				t.Fatalf("ScanUnicodeEscapeRun(%q, start=%d) = (%d, %v), reference (%d, %v)",
					in, start, gotEnd, gotOK, wantEnd, wantOK)
			}
		}
	}
}
