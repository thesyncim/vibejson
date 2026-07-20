package simdjson

import (
	"strings"
	"testing"
)

// escapeStraddlePatterns collects escape shapes whose handling depends on
// carry state across SIMD block boundaries: backslash runs of both parities,
// short escapes, unicode escapes, surrogate pairs, and their truncations.
func escapeStraddlePatterns() []string {
	patterns := []string{
		`\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`,
		`A`, `é`, ` `, `￿`, `𝄞`, `􏿿`,
		`\uD834`, `\uDD1E`, `\uD834A`, `\uD834\uD834`,
		`\x41`, `\ `, `\u00`, `\uZZZZ`, `\U0041`,
	}
	for run := 1; run <= 10; run++ {
		backslashes := strings.Repeat(`\`, run)
		patterns = append(patterns, backslashes+`x`, backslashes+`"`, backslashes+`t`)
	}
	return patterns
}

// TestEscapeBoundaryStraddles places every escape pattern at every offset
// across two 64-byte blocks, in the middle and at the very end of a string
// token, and checks the validators against the reference walker.
func TestEscapeBoundaryStraddles(t *testing.T) {
	patterns := escapeStraddlePatterns()
	pad := strings.Repeat("a", 130)
	for _, pattern := range patterns {
		for prefix := 0; prefix <= 130; prefix++ {
			middle := []byte(`"` + pad[:prefix] + pattern + `tail"`)
			if got, want := validString(middle), stringTokenOracle(middle); got != want {
				t.Fatalf("validString(%q at prefix %d, middle) = %v, want %v", pattern, prefix, got, want)
			}
			ending := []byte(`"` + pad[:prefix] + pattern + `"`)
			if got, want := validString(ending), stringTokenOracle(ending); got != want {
				t.Fatalf("validString(%q at prefix %d, ending) = %v, want %v", pattern, prefix, got, want)
			}
			doc := append(append([]byte(`{"key":`), middle...), '}')
			if got, want := Valid(doc), strictJSONValid(doc); got != want {
				t.Fatalf("Valid(%q at prefix %d, object) = %v, want %v", pattern, prefix, got, want)
			}
		}
	}
}

// TestValidBitmapEscapePhases splices the escape patterns into a
// bitmap-engine document at every 64-byte block phase, exercising the
// engine's escape carry chain and cross-block surrogate pairing against the
// scalar validator and the reference oracle.
func TestValidBitmapEscapePhases(t *testing.T) {
	doc, padStart, padEnd := buildBitmapUTF8Document(t)
	patched := make([]byte, len(doc))
	for _, pattern := range escapeStraddlePatterns() {
		patternWant := false
		for offset := padStart; offset+len(pattern) <= padEnd && offset < padStart+64; offset++ {
			copy(patched, doc)
			copy(patched[offset:], pattern)
			if !testing.Short() || offset == padStart {
				patternWant = strictJSONValid(patched)
			}
			want := patternWant
			got, decided := validBitmap(patched)
			if !decided {
				t.Fatalf("engine declined %q at offset %d", pattern, offset)
			}
			if got != want {
				t.Fatalf("validBitmap(%q at phase %d) = %v, want %v", pattern, offset%64, got, want)
			}
			if !testing.Short() || offset == padStart {
				if scalar := validateOptions(patched, Options{}); (scalar == nil) != want {
					t.Fatalf("scalar Validate(%q at offset %d) = %v, want valid %v", pattern, offset, scalar, want)
				}
			}
		}
	}
}
