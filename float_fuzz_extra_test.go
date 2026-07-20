package simdjson

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// composeNumberText builds a JSON number spelling from raw fuzzer material so
// the fuzzer explores the digit/exponent shape space directly rather than
// waiting for the mutator to stumble onto valid numbers. Every returned string
// is a syntactically valid JSON number.
func composeNumberText(neg bool, intPart, fracPart string, hasFrac bool, exp int, hasExp bool) string {
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	// A JSON number needs a nonempty integer part with no leading zero unless it
	// is exactly "0".
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		b.WriteByte('0')
	} else {
		b.WriteString(intPart)
	}
	if hasFrac {
		if fracPart == "" {
			fracPart = "0"
		}
		b.WriteByte('.')
		b.WriteString(fracPart)
	}
	if hasExp {
		b.WriteByte('e')
		b.WriteString(strconv.Itoa(exp))
	}
	return b.String()
}

// onlyDigits keeps the digit bytes of s and caps the length so composed numbers
// stay in the range JSON parsers actually see.
func onlyDigits(s string, max int) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < max; i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// checkFloatDocumentViews covers the lazy owning and borrowed number views.
// The shared exactness oracle covers the scalar, typed, and dynamic paths.
func checkFloatDocumentViews(t testing.TB, text string) {
	t.Helper()
	want, wantOK := floatOracle64(text)
	src := []byte(text)

	value, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", clip(text), err)
	}
	parsed, parsedOK := value.Float64()
	if parsedOK != wantOK {
		t.Fatalf("Value.Float64(%q) accept = %v, strconv accept = %v", clip(text), parsedOK, wantOK)
	}
	if wantOK && math.Float64bits(parsed) != want {
		t.Fatalf("Value.Float64(%q) = %.17g (%#x), want %#x", clip(text), parsed, math.Float64bits(parsed), want)
	}

	raw, found, err := GetRaw(src, "")
	if err != nil || !found {
		t.Fatalf("GetRaw(%q) = found %v, error %v", clip(text), found, err)
	}
	rawFloat, rawOK := raw.Float64()
	if rawOK != wantOK {
		t.Fatalf("RawValue.Float64(%q) accept = %v, strconv accept = %v", clip(text), rawOK, wantOK)
	}
	if wantOK && math.Float64bits(rawFloat) != want {
		t.Fatalf("RawValue.Float64(%q) = %.17g (%#x), want %#x", clip(text), rawFloat, math.Float64bits(rawFloat), want)
	}
}

// FuzzFloatRoundTripMarshalDecode Marshals a float64 (and its float32 view)
// through this library, then decodes the produced JSON back through the same
// library and through strconv, confirming both recover the exact bits. Marshal
// must emit a shortest round-tripping decimal, and every decode path must
// recover it, so this closes the encode/decode loop over the full bit space
// including wide exponents and subnormals.
func FuzzFloatRoundTripMarshalDecode(f *testing.F) {
	seeds := []uint64{
		0x0000000000000000, 0x8000000000000000, // +0 -0
		0x0000000000000001, 0x000fffffffffffff, // smallest subnormal, largest subnormal
		0x0010000000000000, 0x7fefffffffffffff, // smallest normal, largest finite
		0x3ff0000000000000, 0x4059000000000000, // 1.0, 100.0
		0x3fb999999999999a, 0x43e158e460913d00, // 0.1, ~2.5e18
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, bits uint64) {
		f64 := math.Float64frombits(bits)
		if math.IsNaN(f64) || math.IsInf(f64, 0) {
			t.Skip()
		}
		type wrap struct {
			F float64 `json:"f"`
		}
		data, err := Marshal(&wrap{F: f64})
		if err != nil {
			t.Fatalf("Marshal(%x) error: %v", bits, err)
		}
		// The emitted text must be a valid shortest round-tripping decimal:
		// strconv reading it back yields identical bits.
		text := extractField(t, data)
		back, perr := strconv.ParseFloat(text, 64)
		if perr != nil {
			t.Fatalf("Marshal produced unparseable float text %q from bits %x: %v", text, bits, perr)
		}
		if math.Float64bits(back) != bits {
			t.Fatalf("round trip via strconv lost bits: %x -> %q -> %x", bits, text, math.Float64bits(back))
		}
		// And our own parseFloat64 must recover the same bits from that text.
		mine, merr := parseFloat64([]byte(text))
		if merr != nil {
			t.Fatalf("parseFloat64(%q) from Marshal error: %v", text, merr)
		}
		if math.Float64bits(mine) != bits {
			t.Fatalf("parseFloat64 round trip lost bits: %x -> %q -> %x", bits, text, math.Float64bits(mine))
		}
	})
}

// extractField pulls the numeric text out of {"f":<num>} produced by Marshal.
func extractField(t *testing.T, data []byte) string {
	t.Helper()
	s := string(data)
	const pre = `{"f":`
	if len(s) < len(pre)+1 || s[:len(pre)] != pre || s[len(s)-1] != '}' {
		t.Fatalf("unexpected Marshal shape: %q", s)
	}
	return s[len(pre) : len(s)-1]
}
