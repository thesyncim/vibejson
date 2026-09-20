package vibejson

import (
	"math"
	"math/rand"
	"strconv"
	"testing"
)

func diffFloat64(t *testing.T, s string) {
	t.Helper()
	want, wantErr := strconv.ParseFloat(s, 64)
	if wantErr != nil {
		return
	}

	if got, err := parseFloat64([]byte(s)); err != nil {
		t.Fatalf("parseFloat64(%q) unexpected error: %v", s, err)
	} else if got != want && !(math.IsNaN(got) && math.IsNaN(want)) {
		t.Fatalf("parseFloat64(%q) = %v (bits %#x), want %v (bits %#x)", s, got, math.Float64bits(got), want, math.Float64bits(want))
	}

	var scalar float64
	if err := Unmarshal([]byte(s), &scalar); err != nil {
		t.Fatalf("Unmarshal float64 %q unexpected error: %v", s, err)
	} else if scalar != want {
		t.Fatalf("Unmarshal float64 %q = %v (bits %#x), want %v (bits %#x)", s, scalar, math.Float64bits(scalar), want, math.Float64bits(want))
	}

	var slice []float64
	arr := "[" + s + "," + s + "]"
	if err := Unmarshal([]byte(arr), &slice); err != nil {
		t.Fatalf("Unmarshal []float64 %q unexpected error: %v", arr, err)
	} else if len(slice) != 2 || slice[0] != want || slice[1] != want {
		t.Fatalf("Unmarshal []float64 %q = %v, want [%v %v]", arr, slice, want, want)
	}

	if v, err := unmarshalAnyForTest([]byte(s)); err != nil {
		t.Fatalf("Unmarshal any %q unexpected error: %v", s, err)
	} else if f, ok := v.(float64); !ok {
		t.Fatalf("Unmarshal any %q returned %T, want float64", s, v)
	} else if f != want {
		t.Fatalf("Unmarshal any %q = %v (bits %#x), want %v (bits %#x)", s, f, math.Float64bits(f), want, math.Float64bits(want))
	}
}

func diffFloat32(t *testing.T, s string) {
	t.Helper()
	want, wantErr := strconv.ParseFloat(s, 32)
	if wantErr != nil {
		return
	}
	var scalar float32
	if err := Unmarshal([]byte(s), &scalar); err != nil {
		t.Fatalf("Unmarshal float32 %q unexpected error: %v", s, err)
	} else if scalar != float32(want) {
		t.Fatalf("Unmarshal float32 %q = %v (bits %#x), want %v (bits %#x)", s, scalar, math.Float32bits(scalar), float32(want), math.Float32bits(float32(want)))
	}
	var slice []float32
	arr := "[" + s + "," + s + "]"
	if err := Unmarshal([]byte(arr), &slice); err != nil {
		t.Fatalf("Unmarshal []float32 %q unexpected error: %v", arr, err)
	} else if len(slice) != 2 || slice[0] != float32(want) || slice[1] != float32(want) {
		t.Fatalf("Unmarshal []float32 %q = %v, want [%v %v]", arr, slice, float32(want), float32(want))
	}
}

func TestFloatPowersOfTen(t *testing.T) {
	for exp := -348; exp <= 348; exp++ {
		for _, mant := range []string{"1", "2", "5", "9", "1234567890123456", "9999999999999999", "12345678901234567", "18446744073709551615"} {
			s := mant + "e" + strconv.Itoa(exp)
			diffFloat64(t, s)
			diffFloat32(t, s)
			diffFloat64(t, "-"+s)
		}
	}
}

func TestFloatMantissaBoundaries(t *testing.T) {
	mants := []string{
		"9007199254740991",  // 2^53-1
		"9007199254740992",  // 2^53
		"9007199254740993",  // 2^53+1 (not representable)
		"18014398509481984", // 2^54
		"1234567890123456",
		"12345678901234567",    // 17 digits
		"123456789012345678",   // 18 digits
		"1234567890123456789",  // 19 digits
		"12345678901234567890", // 20 digits
		"99999999999999999999",
		"10000000000000000001",
		"18446744073709551615", // 2^64-1
		"18446744073709551616", // 2^64
		"9999999999999998",
		"9999999999999999",
	}
	for _, m := range mants {
		for exp := -30; exp <= 30; exp++ {
			diffFloat64(t, m+"e"+strconv.Itoa(exp))
			diffFloat64(t, "-"+m+"e"+strconv.Itoa(exp))
			diffFloat64(t, m+"."+m)
		}
	}
}

func TestFloatSpecializedShapes(t *testing.T) {
	r := rand.New(rand.NewSource(0xF10A7))
	for i := 0; i < testIterations(200_000, 2_000); i++ {
		switch i % 5 {
		case 0:
			s := strconv.Itoa(10+r.Intn(90)) + "." + randDigits(r, 1+r.Intn(25))
			diffFloat64(t, s)
			diffFloat32(t, s)
		case 1:
			s := strconv.Itoa(100+r.Intn(900)) + "." + randDigits(r, 1+r.Intn(25))
			diffFloat64(t, s)
			diffFloat32(t, s)
		case 2:
			s := "0." + randZeros(r) + randDigits(r, 1+r.Intn(25))
			diffFloat64(t, s)
			diffFloat32(t, s)
		case 3:
			s := randDigits(r, 1+r.Intn(22)) + "e" + strconv.Itoa(r.Intn(700)-350)
			diffFloat64(t, s)
		case 4:
			f := math.Float64frombits(r.Uint64())
			if math.IsInf(f, 0) || math.IsNaN(f) {
				continue
			}
			s := strconv.FormatFloat(f, 'g', -1, 64)
			diffFloat64(t, s)
			s = strconv.FormatFloat(f, 'e', r.Intn(20), 64)
			diffFloat64(t, s)
		}
	}
}

func TestFloatSubnormals(t *testing.T) {
	for _, s := range []string{
		"5e-324", "4.9e-324", "2.5e-324", "1e-323", "2e-308", "2.2250738585072014e-308",
		"2.2250738585072011e-308", "1.7976931348623157e308", "1.7976931348623159e308",
		"4.940656458412465441765687928682213723651e-324",
		"7.4109846876186981626485318930233205854758970392148714663837852375101326090531312779794975454245398856969484704316857659638998506553390969459816219401617281718945106978546710679176872575177347315553307795408549809608457500958111373034747658096871009590975442271004757307809711118935784838675653998783503015228055934046593739791790738723868299395818481660169122019456499931289798411362062484498678713572180352209017023903285791732520220528974020802906854021606612375549983402671300035812486479041385743401875520901590172592547146296175134159774938718574737870961645638908718119841271673056017045493004705269590165763776884908267986972573366521765567941072508764337560846003984904972149117463085539556354188641513168478436313080237596295773983001708984375e-308",
	} {
		diffFloat64(t, s)
		diffFloat64(t, "-"+s)
	}
}

func randDigits(r *rand.Rand, n int) string {
	b := make([]byte, n)
	b[0] = byte('1' + r.Intn(9))
	for i := 1; i < n; i++ {
		b[i] = byte('0' + r.Intn(10))
	}
	return string(b)
}

func randZeros(r *rand.Rand) string {
	n := r.Intn(6)
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}
	return string(b)
}
