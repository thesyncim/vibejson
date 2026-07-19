package simdjson

import (
	"math"
	"math/rand"
	"strconv"
	"testing"
)

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
