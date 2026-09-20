package vibejson

import (
	"math"
	"strconv"
	"testing"
)

func TestLazyScalarKernelParity(t *testing.T) {
	spellings := []string{
		"0", "-0", "1", "-1", "10", "100",
		"9223372036854775807",  // math.MaxInt64
		"9223372036854775806",  // MaxInt64 - 1
		"9223372036854775808",  // MaxInt64 + 1, overflows int64
		"-9223372036854775808", // math.MinInt64
		"-9223372036854775807", // MinInt64 + 1
		"-9223372036854775809", // MinInt64 - 1, overflows int64
		"18446744073709551615", // math.MaxUint64
		"18446744073709551616", // MaxUint64 + 1
		"99999999999999999999", // 20 digits, past the fold window
		"123456789012345678901234567890",

		"0.0", "-0.0", "0e0", "-0e0", "0.0000", "0e-999",

		"3.14159", "-3.14159", "2.5", "0.5", "-0.5",
		"1.5e3", "1.5E3", "1.5e+3", "1.5e-3", "-1.5e-3",
		"1e10", "1e-10", "1e22", "1e-22", "1e23", "1e-23",

		"5e-324",   // smallest positive subnormal
		"2.5e-324", // rounds to the smallest subnormal
		"1e-323",
		"2.2250738585072014e-308", // smallest positive normal
		"2.2250738585072011e-308", // largest subnormal below it
		"4.9406564584124654e-324", // smallest subnormal, decimal spelling

		"1.7976931348623157e308", // math.MaxFloat64
		"1.7976931348623159e308", // just above, still finite after rounding
		"1.8e308",                // overflows to +Inf
		"1e308", "1e309",
		"1e999", "-1e999", "1e400", "-1e400",

		"1234567890123456789",  // 19 digits
		"12345678901234567890", // 20 digits
		"1.234567890123456789e5",
		"1.2345678901234567890e5",
		"9007199254740993", // 2^53 + 1, not exactly representable
		"9007199254740992", // 2^53
		"9007199254740994", // 2^53 + 2, exactly representable

		"4503599627370496", // 2^52
		"4503599627370495.5",
		"4503599627370497",
		"12345678901234567e22",
		"12345678901234567e-22",
		"1234567890123456.7",

		"-73.9857",
		"40.7484405",
		"179.99999999999999",
		"0.30000000000000004",
	}

	for _, s := range spellings {
		wantF, wantFErr := strconv.ParseFloat(s, 64)
		wantFOK := wantFErr == nil
		wantI, wantIErr := strconv.ParseInt(s, 10, 64)
		wantIOK := wantIErr == nil
		wantU, wantUErr := strconv.ParseUint(s, 10, 64)
		wantUOK := wantUErr == nil

		v, err := Parse([]byte(s))
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		node := v.Node()

		raw, ok, err := GetRaw([]byte(s), "")
		if err != nil || !ok {
			t.Fatalf("GetRaw(%q): ok=%v err=%v", s, ok, err)
		}

		if gotF, gotOK := node.Float64(); gotOK != wantFOK || (wantFOK && !bitEqual(gotF, wantF)) {
			t.Fatalf("Node.Float64(%q) = (%v %#x, %v), want (%v %#x, %v)",
				s, gotF, math.Float64bits(gotF), gotOK, wantF, math.Float64bits(wantF), wantFOK)
		}
		if gotF, gotOK := raw.Float64(); gotOK != wantFOK || (wantFOK && !bitEqual(gotF, wantF)) {
			t.Fatalf("RawValue.Float64(%q) = (%v %#x, %v), want (%v %#x, %v)",
				s, gotF, math.Float64bits(gotF), gotOK, wantF, math.Float64bits(wantF), wantFOK)
		}

		if gotI, gotOK := node.Int64(); gotOK != wantIOK || (wantIOK && gotI != wantI) {
			t.Fatalf("Node.Int64(%q) = (%v, %v), want (%v, %v)", s, gotI, gotOK, wantI, wantIOK)
		}
		if gotI, gotOK := raw.Int64(); gotOK != wantIOK || (wantIOK && gotI != wantI) {
			t.Fatalf("RawValue.Int64(%q) = (%v, %v), want (%v, %v)", s, gotI, gotOK, wantI, wantIOK)
		}

		if gotU, gotOK := node.Uint64(); gotOK != wantUOK || (wantUOK && gotU != wantU) {
			t.Fatalf("Node.Uint64(%q) = (%v, %v), want (%v, %v)", s, gotU, gotOK, wantU, wantUOK)
		}
		if gotU, gotOK := raw.Uint64(); gotOK != wantUOK || (wantUOK && gotU != wantU) {
			t.Fatalf("RawValue.Uint64(%q) = (%v, %v), want (%v, %v)", s, gotU, gotOK, wantU, wantUOK)
		}
	}
}

func bitEqual(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b)
}
