package slopjson

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"testing"
	"unsafe"
)

var parsedDigitsSink uint64
var benchmarkFloatSink float64

func TestLiteralWordLoadsRespectSliceBounds(t *testing.T) {
	src := []byte("xnulltruefalsey")
	if !literalNullAt(src, 1) || !literalTrueAt(src, 5) || !literalFalseTailAt(src, 9) {
		t.Fatal("complete embedded literal was not recognized")
	}
	if literalNullAt(src, -1) || literalTrueAt(src, -1) || literalFalseTailAt(src, -1) {
		t.Fatal("negative literal offset was accepted")
	}
	if literalNullAt(src[:4], 1) || literalTrueAt(src[:8], 5) || literalFalseTailAt(src[:13], 9) {
		t.Fatal("truncated literal was accepted")
	}
	if literalNullAt([]byte("nUll"), 0) || literalTrueAt([]byte("truE"), 0) || literalFalseTailAt([]byte("falsE"), 0) {
		t.Fatal("mismatched literal was accepted")
	}
}

func TestScanNumberFastTaggedSWARMatchesScalar(t *testing.T) {
	cases := []string{
		"0", "-0", "1", "10", "12345678", "123456789", "1234567890",
		"-12345678901234567890", "1.0", "-12.345678901234", "123456789.25",
		"1e2", "1E-20", "123456789e+100", "01", "00", "-", "1.", "1e", "1e+", ".1", "true",
	}
	check := func(data []byte) {
		t.Helper()
		base := unsafe.Pointer(unsafe.SliceData(data))
		wantEnd, wantInt, wantOK := scanNumberFastTagged(base, len(data), 0)
		gotEnd, gotInt, gotOK := scanNumberFastTaggedSWAR(base, len(data), 0)
		if gotEnd != wantEnd || gotInt != wantInt || gotOK != wantOK {
			t.Fatalf("scanNumberFastTaggedSWAR(%q) = (%d,%v,%v), scalar (%d,%v,%v)",
				data, gotEnd, gotInt, gotOK, wantEnd, wantInt, wantOK)
		}
	}
	for _, text := range cases {
		check([]byte(text))
	}

	const alphabet = "0123456789+-.eE,xtruefalsn"
	x := uint64(0x9e3779b97f4a7c15)
	var random [64]byte
	for round := 0; round < 1_000_000; round++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		n := 1 + int(x&63)
		data := random[:n]
		for i := range data {
			x = x*6364136223846793005 + 1442695040888963407
			data[i] = alphabet[x%uint64(len(alphabet))]
		}
		check(data)
	}
}

func TestScanDigitsLongMatchesGeneralScanner(t *testing.T) {
	for digits := 8; digits <= 96; digits++ {
		for _, suffix := range []byte{',', '.', 'e', 'x'} {
			data := make([]byte, digits+1)
			for i := 0; i < digits; i++ {
				data[i] = byte('0' + i%10)
			}
			data[digits] = suffix
			base := unsafe.Pointer(unsafe.SliceData(data))
			if got, want := scanDigitsLong(base, len(data), 0), scanDigitsFast(base, len(data), 0); got != want {
				t.Fatalf("%d digits + %q: long scanner %d, general %d", digits, suffix, got, want)
			}
		}
	}
}

func TestScanTypedFloat64LeadingZerosMatchesStrconv(t *testing.T) {
	for _, text := range []string{
		"0.0006988752666567719",
		"-0.0011574074074074073",
		"0.00012874983906270118",
		"0.028647215558761104",
	} {
		src := []byte(text + ",")
		end, got, exact, ok := scanTypedFloat64(unsafe.Pointer(unsafe.SliceData(src)), len(src), 0)
		if !ok || !exact || end != len(text) {
			t.Fatalf("scanTypedFloat64(%q) = end %d, exact %v, ok %v", text, end, exact, ok)
		}
		want, err := strconv.ParseFloat(text, 64)
		if err != nil {
			t.Fatal(err)
		}
		if math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("scanTypedFloat64(%q) = %.17g (%#x), want %.17g (%#x)",
				text, got, math.Float64bits(got), want, math.Float64bits(want))
		}
	}
}

func TestScanTypedFloat64FormattedValues(t *testing.T) {
	state := uint64(0x243f6a8885a308d3)
	for range 50000 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		want := math.Float64frombits(state)
		if math.IsNaN(want) || math.IsInf(want, 0) {
			continue
		}
		text := strconv.FormatFloat(want, 'g', -1, 64)
		src := []byte(text + ",")
		end, got, exact, ok := scanTypedFloat64(unsafe.Pointer(unsafe.SliceData(src)), len(src), 0)
		if !ok || end != len(text) {
			t.Fatalf("scanTypedFloat64(%q) = end %d, ok %v", text, end, ok)
		}
		if exact && math.Float64bits(got) != math.Float64bits(want) {
			t.Fatalf("scanTypedFloat64(%q) = %.17g (%#x), want %.17g (%#x)",
				text, got, math.Float64bits(got), want, math.Float64bits(want))
		}
	}
}

func TestScanTypedFloat64GeographicShapesMatchStrconv(t *testing.T) {
	for _, text := range []string{
		"12.12345678",
		"12.1234567890123",
		"65.61361699999998",
		"-43.42027300000001",
		"99.1234567890123456",
		"100.12345678",
		"141.00299000000003",
		"-179.9999999999999",
		"100.123456789012345",
		"12.12345678901234e+2",
		"-141.00299000000003e-2",
	} {
		for _, delimiter := range []byte{',', ']', '}'} {
			src := append([]byte(text), delimiter)
			end, got, exact, ok := scanTypedFloat64(unsafe.Pointer(unsafe.SliceData(src)), len(src), 0)
			if !ok || !exact || end != len(text) {
				t.Fatalf("scanTypedFloat64(%q) = end %d, exact %v, ok %v", text, end, exact, ok)
			}
			want, err := strconv.ParseFloat(text, 64)
			if err != nil {
				t.Fatal(err)
			}
			if math.Float64bits(got) != math.Float64bits(want) {
				t.Fatalf("scanTypedFloat64(%q) = %.17g (%#x), want %.17g (%#x)",
					text, got, math.Float64bits(got), want, math.Float64bits(want))
			}
		}
	}
}

func TestScaleJSONFloat64FixedMatchesGeneric(t *testing.T) {
	tests := []struct {
		exponent     int
		powerHi      uint64
		powerLo      uint64
		exponentBias int
		minimum      uint64
		span         uint64
	}{
		{-16, 0xe69594bec44de15c, 0xb3d141978676564c, 968, 1e17, 18e17},
		{-15, 0x901d7cf73ab0acda, 0xf062c8feb409f5ef, 972, 1e17, 18e17},
	}
	for _, tc := range tests {
		state := uint64(0x9e3779b97f4a7c15)
		for range 100000 {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			mantissa := tc.minimum + state%tc.span
			for _, negative := range []bool{false, true} {
				want, ok := scaleJSONFloat64(mantissa, tc.exponent, negative)
				if !ok {
					t.Fatalf("scaleJSONFloat64(%d, %d, %v) was not exact", mantissa, tc.exponent, negative)
				}
				got := scaleJSONFloat64Fixed(
					mantissa, tc.powerHi, tc.powerLo, tc.exponentBias, negative,
				)
				if math.Float64bits(got) != math.Float64bits(want) {
					t.Fatalf("fixed scale of %d*10^%d = %.17g (%#x), want %.17g (%#x)",
						mantissa, tc.exponent, got, math.Float64bits(got), want, math.Float64bits(want))
				}
			}
		}
	}
}

func TestParse8Digits(t *testing.T) {
	for value := uint64(0); value < 100000000; value += 7919 {
		text := []byte(fmt.Sprintf("%08d", value))
		base := unsafe.Pointer(unsafe.SliceData(text))
		if !all8Digits(base) {
			t.Fatalf("all8Digits rejected %q", text)
		}
		if got := parse8Digits(base); got != value {
			t.Fatalf("parse8Digits(%q) = %d, want %d", text, got, value)
		}
	}
	text := []byte("00000000")
	for i := range text {
		for value := 0; value < 256; value++ {
			text[i] = byte(value)
			want := value >= '0' && value <= '9'
			if got := all8Digits(unsafe.Pointer(unsafe.SliceData(text))); got != want {
				t.Fatalf("position %d byte %#02x: all8Digits = %v, want %v", i, value, got, want)
			}
		}
		text[i] = '0'
	}
}

func TestAll16DigitsEveryBytePosition(t *testing.T) {
	var digits [16]byte
	for i := range digits {
		digits[i] = '0'
	}
	base := unsafe.Pointer(&digits[0])
	if !all16Digits(base) {
		t.Fatal("all16Digits rejected zero digits")
	}
	for i := range digits {
		for value := 0; value < 256; value++ {
			digits[i] = byte(value)
			want := value >= '0' && value <= '9'
			if got := all16Digits(base); got != want {
				t.Fatalf("position %d byte %#02x: all16Digits = %v, want %v", i, value, got, want)
			}
		}
		digits[i] = '0'
	}
}

func TestScanDigitsFast(t *testing.T) {
	for prefix := 0; prefix <= 40; prefix++ {
		for suffix := 0; suffix <= 16; suffix++ {
			text := make([]byte, prefix+1+suffix)
			for i := range text {
				text[i] = '7'
			}
			text[prefix] = ','
			base := unsafe.Pointer(unsafe.SliceData(text))
			for start := 0; start <= prefix; start++ {
				if got := scanDigitsFast(base, len(text), start); got != prefix {
					t.Fatalf("prefix=%d suffix=%d start=%d: got %d", prefix, suffix, start, got)
				}
			}
		}
	}

	var bytes [16]byte
	for i := range bytes {
		bytes[i] = '0'
	}
	base := unsafe.Pointer(&bytes[0])
	for position := range bytes {
		for value := 0; value < 256; value++ {
			bytes[position] = byte(value)
			want := position
			if value >= '0' && value <= '9' {
				want = len(bytes)
			}
			if got := scanDigitsFast(base, len(bytes), 0); got != want {
				t.Fatalf("position=%d value=%#02x: got %d, want %d", position, value, got, want)
			}
		}
		bytes[position] = '0'
	}
}

func TestParse16Digits(t *testing.T) {
	for _, text := range []string{
		"0000000000000000",
		"0000000000000001",
		"0123456789012345",
		"1000000000000000",
		"9007199254740992",
		"9999999999999999",
	} {
		checkParse16Digits(t, []byte(text))
	}

	state := uint64(0x9e3779b97f4a7c15)
	var digits [16]byte
	for range 10000 {
		for i := range digits {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			digits[i] = '0' + byte(state%10)
		}
		checkParse16Digits(t, digits[:])
	}
}

func checkParse16DigitsText(t *testing.T, text string) {
	t.Helper()
	if len(text) != 16 {
		return
	}
	for i := range text {
		if text[i] < '0' || text[i] > '9' {
			return
		}
	}
	checkParse16Digits(t, []byte(text))
}

// parse16DigitsScalar is the portable reference the kernel is checked
// against; it lives with its only callers.
func parse16DigitsScalar(base unsafe.Pointer) uint64 {
	var value uint64
	for i := 0; i < 16; i++ {
		value = value*10 + uint64(fastByteAt(base, i)-'0')
	}
	return value
}

func checkParse16Digits(t testing.TB, digits []byte) {
	t.Helper()
	base := unsafe.Pointer(unsafe.SliceData(digits))
	want, err := strconv.ParseUint(string(digits), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if got := parse16DigitsScalar(base); got != want {
		t.Fatalf("parse16DigitsScalar(%q) = %d, want %d", digits, got, want)
	}
	if got := parse16Digits(base); got != want {
		t.Fatalf("parse16Digits(%q) = %d, want %d", digits, got, want)
	}
}

func BenchmarkParse16Digits(b *testing.B) {
	digits := []byte("1234567890123456")
	base := unsafe.Pointer(unsafe.SliceData(digits))
	b.Run("selected", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			parsedDigitsSink = parse16Digits(base)
		}
	})
	b.Run("scalar", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			parsedDigitsSink = parse16DigitsScalar(base)
		}
	})
}

func BenchmarkParseFloat64(b *testing.B) {
	for _, text := range []string{
		"2.5",
		"1234567890123456",
		"1234567890123456.25",
		"1.2345678901234567e-120",
	} {
		data := []byte(text)
		b.Run(text+"/SLOPJSON", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				value, err := parseFloat64(data)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkFloatSink = value
			}
		})
		b.Run(text+"/strconv", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				value, err := strconv.ParseFloat(text, 64)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkFloatSink = value
			}
		})
		b.Run(text+"/encoding-json", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := json.Unmarshal(data, &benchmarkFloatSink); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
