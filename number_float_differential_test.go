package vibejson

import (
	"math"
	"math/rand"
	"strconv"
	"testing"
)

func TestFloatHeavy(t *testing.T) {
	seeds := int64(testIterations(8, 1))
	iterations := testIterations(500_000, 5_000)
	for seed := int64(1); seed <= seeds; seed++ {
		r := rand.New(rand.NewSource(seed * 0x1E3779B97F4A7C15))
		for i := 0; i < iterations; i++ {
			s := randFloatString(r)
			diffFloat64(t, s)
			if i%4 == 0 {
				diffFloat32(t, s)
			}
		}
	}
}

func randFloatString(r *rand.Rand) string {
	switch r.Intn(10) {
	case 0:
		f := math.Float64frombits(r.Uint64())
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return "0"
		}
		return strconv.FormatFloat(f, byte("gef"[r.Intn(3)]), r.Intn(21)-1, 64)
	case 1, 2:
		intLen := 1 + r.Intn(20)
		fracLen := 1 + r.Intn(22)
		return randDigits(r, intLen) + "." + randDigitsAny(r, fracLen)
	case 3:
		return "0." + randZeros(r) + randDigitsAny(r, 1+r.Intn(24))
	case 4, 5:
		return randDigits(r, 1+r.Intn(21)) + eOrE(r) + expStr(r)
	case 6:
		return randDigits(r, 1+r.Intn(19)) + "." + randDigitsAny(r, 1+r.Intn(19)) + eOrE(r) + expStr(r)
	case 7:
		return randDigits(r, 17) + eOrE(r) + strconv.Itoa(r.Intn(40)-20)
	case 8:
		return randDigits(r, 1+r.Intn(19)) + "e" + strconv.Itoa(r.Intn(60)-360+r.Intn(2)*700)
	default:
		return strconv.Itoa(r.Intn(1000000)) + "." + randDigitsAny(r, 8+r.Intn(12))
	}
}

func randDigitsAny(r *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('0' + r.Intn(10))
	}
	return string(b)
}

func eOrE(r *rand.Rand) string {
	if r.Intn(2) == 0 {
		return "e"
	}
	return "E"
}

func expStr(r *rand.Rand) string {
	v := r.Intn(760) - 380
	s := strconv.Itoa(v)
	if v >= 0 && r.Intn(2) == 0 {
		s = "+" + s
	}
	return s
}
