package vibejson

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func composeNumberText(neg bool, intPart, fracPart string, hasFrac bool, exp int, hasExp bool) string {
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
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

func onlyDigits(s string, max int) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < max; i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

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
		text := extractField(t, data)
		back, perr := strconv.ParseFloat(text, 64)
		if perr != nil {
			t.Fatalf("Marshal produced unparseable float text %q from bits %x: %v", text, bits, perr)
		}
		if math.Float64bits(back) != bits {
			t.Fatalf("round trip via strconv lost bits: %x -> %q -> %x", bits, text, math.Float64bits(back))
		}
		mine, merr := parseFloat64([]byte(text))
		if merr != nil {
			t.Fatalf("parseFloat64(%q) from Marshal error: %v", text, merr)
		}
		if math.Float64bits(mine) != bits {
			t.Fatalf("parseFloat64 round trip lost bits: %x -> %q -> %x", bits, text, math.Float64bits(mine))
		}
	})
}

func extractField(t *testing.T, data []byte) string {
	t.Helper()
	s := string(data)
	const pre = `{"f":`
	if len(s) < len(pre)+1 || s[:len(pre)] != pre || s[len(s)-1] != '}' {
		t.Fatalf("unexpected Marshal shape: %q", s)
	}
	return s[len(pre) : len(s)-1]
}
