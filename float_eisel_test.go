package simdjson

import (
	"math"
	"math/rand"
	"strconv"
	"testing"
)

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
