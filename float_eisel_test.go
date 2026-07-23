package slopjson

import (
	"math"
	"math/rand"
	"strconv"
	"testing"
)

// Mirror the generated kernel's intentionally fixed exponent coverage without
// exporting its table bounds from the internal package solely for tests.
const detailedPowersOfTenMinExp10 = -348
const detailedPowersOfTenMaxExp10 = 347

// eiselLemireOracle formats man*10^exp10 the way strconv parses it and returns
// the correctly rounded float64, or ok=false when the value is not a finite
// normal number (overflow, subnormal, or zero) — exactly the cases in which
// eiselLemire64 defers to the slow path and so must not be compared.
func eiselLemireOracle(man uint64, exp10 int, neg bool) (float64, bool) {
	text := strconv.FormatUint(man, 10) + "e" + strconv.Itoa(exp10)
	if neg {
		text = "-" + text
	}
	ref, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(ref, 0) || ref == 0 {
		return 0, false
	}
	// Subnormals fall outside eiselLemire64's normal-number envelope.
	if math.Abs(ref) < math.SmallestNonzeroFloat64*(1<<52) {
		return 0, false
	}
	return ref, true
}

// checkEiselLemire asserts that whenever eiselLemire64 commits to a result it
// matches strconv bit-for-bit. A deferral (ok=false) is always acceptable: the
// production caller falls back to strconv there.
func checkEiselLemire(t *testing.T, man uint64, exp10 int, neg bool) {
	t.Helper()
	got, ok := eiselLemire64(man, exp10, neg)
	if !ok {
		return
	}
	ref, refOK := eiselLemireOracle(man, exp10, neg)
	if !refOK {
		return
	}
	if math.Float64bits(got) != math.Float64bits(ref) {
		t.Fatalf("eiselLemire64(%d, %d, %v) = %x (%v), strconv = %x (%v)",
			man, exp10, neg, math.Float64bits(got), got, math.Float64bits(ref), ref)
	}
}

// TestEiselLemireMatchesStrconvSweep drives every tabulated exponent with a set
// of mantissas that spans the significand widths, guaranteeing every table
// entry is exercised against strconv.
func TestEiselLemireMatchesStrconvSweep(t *testing.T) {
	mantissas := []uint64{
		1, 2, 3, 5, 7, 9, 10, 11, 99, 100, 123, 999, 1000,
		1<<52 - 1, 1 << 52, 1<<52 + 1, 1<<53 - 1, 1 << 53,
		9007199254740993, 9999999999999999, 12345678901234567,
		1<<63 - 1, 1 << 63, math.MaxUint64, math.MaxUint64 - 1,
		9223372036854775807, 4611686018427387904,
	}
	for exp10 := detailedPowersOfTenMinExp10; exp10 <= detailedPowersOfTenMaxExp10; exp10++ {
		for _, man := range mantissas {
			checkEiselLemire(t, man, exp10, false)
			checkEiselLemire(t, man, exp10, true)
		}
	}
}

// TestEiselLemireMatchesStrconvRandom hammers random (mantissa, exponent) pairs
// across the whole range, including full-width mantissas that force the 128-bit
// fold-in branch.
func TestEiselLemireMatchesStrconvRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(0xf10a7))
	span := detailedPowersOfTenMaxExp10 - detailedPowersOfTenMinExp10 + 1
	for i := 0; i < testIterations(4_000_000, 20_000); i++ {
		exp10 := detailedPowersOfTenMinExp10 + rng.Intn(span)
		var man uint64
		switch i % 3 {
		case 0:
			man = uint64(rng.Int63n(1 << 53))
		case 1:
			man = rng.Uint64()
		default:
			man = uint64(rng.Intn(1_000_000_000))
		}
		checkEiselLemire(t, man, exp10, i&1 == 0)
	}
}

// TestEiselLemireRoundTripsFloats confirms that formatting a float and feeding
// its shortest decimal back through eiselLemire64 recovers the exact bits, the
// property real JSON decoding depends on.
func TestEiselLemireRoundTripsFloats(t *testing.T) {
	rng := rand.New(rand.NewSource(0xba5e))
	for i := 0; i < testIterations(2_000_000, 10_000); i++ {
		bits := rng.Uint64()
		f := math.Float64frombits(bits)
		if math.IsNaN(f) || math.IsInf(f, 0) || f == 0 {
			continue
		}
		text := strconv.FormatFloat(f, 'e', -1, 64)
		man, exp10, neg, ok := parseDecimalForTest(text)
		if !ok {
			continue
		}
		got, elOK := eiselLemire64(man, exp10, neg)
		if !elOK {
			continue
		}
		if math.Float64bits(got) != math.Float64bits(f) {
			t.Fatalf("round trip %q: eiselLemire64=%x want %x", text, math.Float64bits(got), math.Float64bits(f))
		}
	}
}

// parseDecimalForTest extracts (mantissa, exp10, negative) from a strconv 'e'
// formatting, collapsing the fraction into the mantissa. It returns ok=false
// when the mantissa would not fit in 64 bits (the truncated case eiselLemire64
// is not asked to handle).
func parseDecimalForTest(text string) (man uint64, exp10 int, neg bool, ok bool) {
	i := 0
	if i < len(text) && text[i] == '-' {
		neg = true
		i++
	}
	fracDigits := 0
	digits := 0
	seenDot := false
	for ; i < len(text); i++ {
		c := text[i]
		switch {
		case c >= '0' && c <= '9':
			if digits >= 19 {
				return 0, 0, false, false
			}
			man = man*10 + uint64(c-'0')
			digits++
			if seenDot {
				fracDigits++
			}
		case c == '.':
			seenDot = true
		case c == 'e' || c == 'E':
			e, err := strconv.Atoi(text[i+1:])
			if err != nil {
				return 0, 0, false, false
			}
			return man, e - fracDigits, neg, true
		default:
			return 0, 0, false, false
		}
	}
	return man, -fracDigits, neg, true
}

// floatDecodeOracle is the correctly rounded value strconv assigns to text; the
// three decode entry points must all match it.
func floatDecodeOracle(t *testing.T, text string) (float64, bool) {
	t.Helper()
	ref, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(ref, 0) {
		return 0, false
	}
	return ref, true
}

// checkFloatDecodePaths pins every float decode entry point — typed decode,
// parseFloat64, and the any/Value path — to strconv for one JSON number, so a
// wiring mistake in the Eisel-Lemire hand-off (sign, exponent, offset) cannot
// slip through even though eiselLemire64 is proven in isolation.
func checkFloatDecodePaths(t *testing.T, text string) {
	t.Helper()
	ref, ok := floatDecodeOracle(t, text)
	if !ok {
		return
	}
	refBits := math.Float64bits(ref)

	if got, err := parseFloat64([]byte(text)); err != nil {
		t.Fatalf("parseFloat64(%q) error: %v", text, err)
	} else if math.Float64bits(got) != refBits {
		t.Fatalf("parseFloat64(%q) = %x want %x", text, math.Float64bits(got), refBits)
	}

	dec, err := CompileDecoder[float64](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var typed float64
	if err := dec.Decode([]byte(text), &typed); err != nil {
		t.Fatalf("typed decode %q error: %v", text, err)
	} else if math.Float64bits(typed) != refBits {
		t.Fatalf("typed decode %q = %x want %x", text, math.Float64bits(typed), refBits)
	}

	v, err := Parse([]byte(text))
	if err != nil {
		t.Fatalf("Parse(%q) error: %v", text, err)
	}
	if got, ok := v.Float64(); !ok {
		t.Fatalf("Value.Float64(%q) not a float", text)
	} else if math.Float64bits(got) != refBits {
		t.Fatalf("Value.Float64(%q) = %x want %x", text, math.Float64bits(got), refBits)
	}
}

// TestFloatDecodeMatchesStrconv exercises the shortest, fixed, and scientific
// spellings of random float64 values — the shapes that miss the exact-multiply
// envelope and now route through Eisel-Lemire instead of strconv.
func TestFloatDecodeMatchesStrconv(t *testing.T) {
	rng := rand.New(rand.NewSource(0xd0d0))
	formats := []struct {
		verb byte
		prec int
	}{
		{'g', -1}, {'e', -1}, {'f', -1},
		{'e', 12}, {'e', 16}, {'f', 14}, {'g', 17},
	}
	for i := 0; i < testIterations(500_000, 5_000); i++ {
		f := math.Float64frombits(rng.Uint64())
		if math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		fm := formats[i%len(formats)]
		text := strconv.FormatFloat(f, fm.verb, fm.prec, 64)
		checkFloatDecodePaths(t, text)
	}
}

// TestFloatDecodeAdversarial covers boundary spellings that historically strain
// decimal-to-binary rounding: long nines, halfway ties, tiny and huge
// magnitudes, and redundant zeros.
func TestFloatDecodeAdversarial(t *testing.T) {
	cases := []string{
		"1.7976931348623157e308", "2.2250738585072014e-308", "5e-324",
		"9007199254740993", "9007199254740992.0", "9999999999999999",
		"1.000000000000000011102230246251565404236316680908203125",
		"0.1", "0.2", "0.3", "0.30000000000000004",
		"123456789012345678901234567890e-15",
		"1e0", "1e1", "1e-1", "1.5e-10", "-3.14159265358979e-22",
		"0.0000000000000000000001", "1000000000000000000000.5",
		"2.5", "-0.0", "3.141592653589793", "2.718281828459045",
		"1.1", "1.2", "1.3", "1.4", "1.6", "1.7", "1.8", "1.9",
		"4.9406564584124654e-324", "1.7976931348623158e308",
	}
	for _, text := range cases {
		checkFloatDecodePaths(t, text)
	}
}
