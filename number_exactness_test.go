package vibejson

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

func floatOracle64(text string) (bits uint64, ok bool) {
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, false
	}
	return math.Float64bits(value), true
}

func floatOracle32(text string) (bits uint32, ok bool) {
	value, err := strconv.ParseFloat(text, 32)
	if err != nil {
		return 0, false
	}
	return math.Float32bits(float32(value)), true
}

func parseFloat64(src []byte) (float64, error) {
	start := SkipSpace(src, 0)
	if start == len(src) {
		return 0, (&parser{src: src}).err(start, "expected number")
	}
	source := numberSource{base: &src[0]}
	base := source.PointerAt(0)
	end, number, ok := scanJSONNumber(base, len(src), start)
	if !ok {
		_, msg := scanNumber(src, start)
		return 0, (&parser{src: src}).err(start, msg)
	}
	if trailing := SkipSpace(src, end); trailing != len(src) {
		return 0, (&parser{src: src}).err(trailing, "unexpected trailing data")
	}
	if value, exact := number.exactFloat64(); exact {
		return value, nil
	}
	if !number.truncated {
		if value, ok := eiselLemire64(number.mantissa, number.exponent, number.negative); ok {
			return value, nil
		}
	}
	text := OwnedBytesString(src[start:end])
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, (&parser{src: src}).err(start, "number out of range")
	}
	return value, nil
}

func checkFloatExactness(t testing.TB, text string) {
	t.Helper()
	src := []byte(text)
	if !validNumber(src) {
		t.Fatalf("checkFloatExactness called with invalid JSON number %q", clip(text))
	}
	want64, ok64 := floatOracle64(text)
	want32, ok32 := floatOracle32(text)

	var stdValue float64
	stdErr := json.Unmarshal(src, &stdValue)
	if (stdErr == nil) != ok64 {
		t.Fatalf("%q: encoding/json accept = %v, strconv accept = %v", clip(text), stdErr == nil, ok64)
	}

	got, err := parseFloat64(src)
	if (err == nil) != ok64 {
		t.Fatalf("%q: parseFloat64 error = %v, strconv accept = %v", clip(text), err, ok64)
	}
	if ok64 && math.Float64bits(got) != want64 {
		t.Fatalf("parseFloat64(%q) = %.17g (%#x), want %.17g (%#x)",
			clip(text), got, math.Float64bits(got), math.Float64frombits(want64), want64)
	}

	checkFloatDecode(t, text, src, want64, ok64, want32, ok32)

	tree, anyErr := unmarshalAnyForTest(src)
	if (anyErr == nil) != ok64 {
		t.Fatalf("%q: Unmarshal any error = %v, strconv accept = %v", clip(text), anyErr, ok64)
	}
	if ok64 {
		value, isFloat := tree.(float64)
		if !isFloat {
			t.Fatalf("Unmarshal any(%q) = %T, want float64", clip(text), tree)
		}
		if math.Float64bits(value) != want64 {
			t.Fatalf("Unmarshal any(%q) = %.17g (%#x), want %#x", clip(text), value, math.Float64bits(value), want64)
		}
	}
}

func checkFloatDecode(t testing.TB, text string, src []byte, want64 uint64, ok64 bool, want32 uint32, ok32 bool) {
	t.Helper()
	var f64 float64
	err := Unmarshal(src, &f64)
	if (err == nil) != ok64 {
		t.Fatalf("%q: Unmarshal float64 error = %v, strconv accept = %v", clip(text), err, ok64)
	}
	if ok64 && math.Float64bits(f64) != want64 {
		t.Fatalf("Unmarshal float64 (%q) = %.17g (%#x), want %#x", clip(text), f64, math.Float64bits(f64), want64)
	}

	var f32 float32
	err = Unmarshal(src, &f32)
	if (err == nil) != ok32 {
		t.Fatalf("%q: Unmarshal float32 error = %v, strconv accept = %v", clip(text), err, ok32)
	}
	if ok32 && math.Float32bits(f32) != want32 {
		t.Fatalf("Unmarshal float32 (%q) = %.9g (%#x), want %#x", clip(text), f32, math.Float32bits(f32), want32)
	}

	for _, wrap := range []struct{ prefix, suffix string }{
		{"[", "]"},
		{"[", ",0]"},
		{"[", " ]"},
		{`{"v":`, "}"},
	} {
		doc := []byte(wrap.prefix + text + wrap.suffix)
		var s64 []float64
		var m64 struct {
			V float64 `json:"v"`
		}
		var got float64
		if wrap.prefix == "[" {
			err = Unmarshal(doc, &s64)
			if err == nil {
				got = s64[0]
			}
		} else {
			err = Unmarshal(doc, &m64)
			got = m64.V
		}
		if (err == nil) != ok64 {
			t.Fatalf("%q in %q%q: decode error = %v, strconv accept = %v", clip(text), wrap.prefix, wrap.suffix, err, ok64)
		}
		if ok64 && math.Float64bits(got) != want64 {
			t.Fatalf("decode %q in %q%q = %.17g (%#x), want %#x",
				clip(text), wrap.prefix, wrap.suffix, got, math.Float64bits(got), want64)
		}

		if wrap.prefix == "[" {
			var s32 []float32
			err = Unmarshal(doc, &s32)
			if (err == nil) != ok32 {
				t.Fatalf("%q in array: float32 decode error = %v, strconv accept = %v", clip(text), err, ok32)
			}
			if ok32 && math.Float32bits(s32[0]) != want32 {
				t.Fatalf("decode float32 %q in array = %.9g (%#x), want %#x",
					clip(text), s32[0], math.Float32bits(s32[0]), want32)
			}
		}
	}
}

func clip(text string) string {
	if len(text) > 64 {
		return text[:64] + "..."
	}
	return text
}

func TestFloatHardCases(t *testing.T) {
	longSubnormalHalfwayDown := "2.22507385850720113605740979670913197593481954635164564802342610972482222202107694551652952390813508791414915891303962110687008643869459464552765720740782062174337998814106326732925355228688137214901298112245145188984905722230728525513315575501591439747639798341180199932396254828901710708185069063066665599493827577257201576306269066333264756530000924588831643303777979186961204949739037782970490505108060994073026293712895895000358379996720725430436028407889577179615094551674824347103070260914462157228988025818254518032570701886087211312807951223342628836862232150377566662250398253433597456888442390026549819838548794829220689472168983109969836584681402285424333066033985088644580400103493397042756718644338377048603786162277173854562306587467901408672332763671875e-308"
	longSubnormalHalfwayUp := "2.22507385850720113605740979670913197593481954635164564802342610972482222202107694551652952390813508791414915891303962110687008643869459464552765720740782062174337998814106326732925355228688137214901298112245145188984905722230728525513315575501591439747639798341180199932396254828901710708185069063066665599493827577257201576306269066333264756530000924588831643303777979186961204949739037782970490505108060994073026293712895895000358379996720725430436028407889577179615094551674824347103070260914462157228988025818254518032570701886087211312807951223342628836862232150377566662250398253433597456888442390026549819838548794829220689472168983109969836584681402285424333066033985088644580400103493397042756718644338377048603786162277173854562306587467901408672332763671876e-308"
	minSubnormalLongForm := "4.9406564584124654417656879286822137236505980261432476442558568250067550727020875186529983636163599237979656469544571773092665671035593979639877479601078187812630071319031140452784581716784898210368871863605699873072305000638063535580581881419625841635267361540715428955555046406705686629002018452550"
	tests := []string{
		"2.2250738585072011e-308",
		"2.2250738585072012e-308",
		"2.2250738585072014e-308",
		"0.00022250738585072011e-304",
		"2.225073858507201136057409796709131975934819546351645648023426109724822222021076945516529523908135087914149158913039621106870086438694594645527657207407820621743379988141063267329253552286881372149012981122451451889849057222307285255133155755015914397476397983411801999323962548289017107081850690630666655994938275772572015763062690663332647565300009245888316433037777979186961204949739037782970490505108060994073026293712895895000358379996720725430436028407889577179615094551674824347103070260914462157228988025818254518032570701886087211312807951223342628836862232150377566662250398253433597456888442390026549819838548794829220689472168983109969836584681402285424333066033985088644580400103493397042756718644338377048603786162277173854562306587467901408672332763671875e-308",
		longSubnormalHalfwayDown,
		longSubnormalHalfwayUp,
		"5e-324",
		"4.9406564584124654e-324",
		minSubnormalLongForm + "e-308",
		"2.4703282292062328e-324",
		"3e-324",
		"1.7976931348623157e308",
		"1.7976931348623158e308",
		"8.98846567431158e307",
		"1e308",
		"9.9e307",
		"123456789012345678901234567890e280",
		"9007199254740992",
		"9007199254740993",
		"9007199254740994",
		"9007199254740993.00000001",
		"9007199254740992.5",
		"9007199254740994.5",
		"1.00000000000000011102230246251565404236316680908203125",
		"1.00000000000000011102230246251565404236316680908203124",
		"1.00000000000000011102230246251565404236316680908203126",
		"0.999999999999999944488848768742172978818416595458984375",
		"1e22",
		"1e23",
		"-1e22",
		"4503599627370496.5",
		"4503599627370497.5",
		"9223372036854775807",
		"9223372036854775808",
		"18446744073709551615",
		"18446744073709551616",
		"999999999999999999999",
		"100000000000000000000000000000000000000000",
		"1234567890123456789.0123456789",
		"0.1234567890123456789012345678901234567890123456789",
		"12345678901234567890e-20",
		"0.000000000000000000000000000000000000000000000000000000000000000001",
		"0.00000000000000001234567890123456",
		"0.0000000000000000123456789012345678",
		"0",
		"-0",
		"0.0",
		"-0.0",
		"0e999999999999999999",
		"-0e-999999999999999999",
		"0.0000e5",
		"0.0e-99999",
		"1e999999999999999999",
		"1e-999999999999999999",
		"1.5e9999",
		"1.5e-9999",
		"1e2147483647",
		"1e-2147483648",
		"1e18446744073709551616",
		"1234567890123456",
		"12345678901234567",
		"1234567890123456.7890123456789012",
		"1234567890123456.7890123456789012e-10",
		"1111111111111111111111111111111111111111",
		"1e0",
		"1E+2",
		"2.5",
		"-2.5e-2",
		"7e9",
		"7e-9",
	}
	for _, text := range tests {
		checkFloatExactness(t, text)
		if !strings.HasPrefix(text, "-") {
			checkFloatExactness(t, "-"+text)
		}
	}
}

func TestFloatShortFormsExhaustive(t *testing.T) {
	var forms []string
	for d := byte('0'); d <= '9'; d++ {
		forms = append(forms, string(d))
		for f := byte('0'); f <= '9'; f++ {
			forms = append(forms, string(d)+"."+string(f))
		}
		for e := byte('0'); e <= '9'; e++ {
			forms = append(forms,
				string(d)+"e"+string(e),
				string(d)+"e+"+string(e),
				string(d)+"e-"+string(e),
				string(d)+"E"+string(e),
				string(d)+"E-"+string(e),
			)
		}
	}
	for _, form := range forms {
		checkFloatExactness(t, form)
		checkFloatExactness(t, "-"+form)
	}
}

func TestFloatFormattedBitPatterns(t *testing.T) {
	iterations := 30000
	if testing.Short() {
		iterations = 2000
	}
	state := uint64(0x853c49e6748fea9b)
	for range iterations {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		value := math.Float64frombits(state)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		checkFloatExactness(t, strconv.FormatFloat(value, 'g', -1, 64))
		checkFloatExactness(t, strconv.FormatFloat(value, 'e', 17, 64))
		if abs := math.Abs(value); abs > 1e-30 && abs < 1e30 {
			checkFloatExactness(t, strconv.FormatFloat(value, 'f', 25, 64))
		}

		narrow := math.Float32frombits(uint32(state))
		if math.IsNaN(float64(narrow)) || math.IsInf(float64(narrow), 0) {
			continue
		}
		checkFloatExactness(t, strconv.FormatFloat(float64(narrow), 'g', -1, 32))
	}
}

func TestFloatRandomDecimalStrings(t *testing.T) {
	iterations := 20000
	if testing.Short() {
		iterations = 2000
	}
	state := uint64(0xda3e39cb94b95bdb)
	next := func(bound uint64) uint64 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return state % bound
	}
	var builder bytes.Buffer
	for range iterations {
		builder.Reset()
		if next(2) == 0 {
			builder.WriteByte('-')
		}
		intDigits := int(next(28)) + 1
		if next(4) == 0 {
			intDigits = 1
		}
		for i := range intDigits {
			if i == 0 && intDigits > 1 {
				builder.WriteByte('1' + byte(next(9)))
				continue
			}
			builder.WriteByte('0' + byte(next(10)))
		}
		if next(2) == 0 {
			builder.WriteByte('.')
			fracDigits := int(next(30)) + 1
			for range fracDigits {
				builder.WriteByte('0' + byte(next(10)))
			}
		}
		if next(3) == 0 {
			builder.WriteByte('e')
			if next(2) == 0 {
				builder.WriteByte('-')
			} else if next(2) == 0 {
				builder.WriteByte('+')
			}
			exponent := int(next(340)) + 1
			builder.WriteString(strconv.Itoa(exponent))
		}
		checkFloatExactness(t, builder.String())
	}
}

func FuzzFloatExactness(f *testing.F) {
	const arbitraryText, structuredNumber byte = 0, 1

	for _, seed := range []string{
		"0000000000000000",
		"0123456789012345",
		"9007199254740992",
		"9999999999999999",
		"0",
		"-0.0",
		"2.2250738585072011e-308",
		"1.7976931348623159e308",
		"9007199254740993",
		"1e999999999999999999",
		"0e999999999999999999",
		"1234567890123456.7890123456789012",
		"5e-324",
		"0.1",
		"1e10",
		"1.5e-8",
		"3.14159e-22",
		"1.7976931348623157e308",
		"2.2250738585072014e-308",
		"0e0",
		"1e400",
		"1e-400",
		"123456789012345678901234567890e-15",
		"0.00000000000000000000000000001",
		"9999999999999999e300",
		"1e",
		"01",
		"+1",
		".5",
		"5.",
		"0x1p3",
		"1_000",
		"Infinity",
		"NaN",
	} {
		f.Add(arbitraryText, seed, false, "", "", false, 0, false)
	}

	f.Add(structuredNumber, "", false, "1", "5", true, 308, true)
	f.Add(structuredNumber, "", true, "9007199254740993", "", false, 0, false)
	f.Add(structuredNumber, "", false, "5", "", false, -324, true)
	f.Add(structuredNumber, "", false, "22250738585072014", "", false, -324, true)
	f.Add(structuredNumber, "", false, "1", "234567890123456789012345", true, -300, true)
	f.Add(structuredNumber, "", false, "0", "1", true, 0, false)
	f.Add(structuredNumber, "", false, "73", "1234567890123", true, 0, false)
	f.Add(structuredNumber, "", true, "73", "12345678901234", true, 0, false)
	f.Add(structuredNumber, "", false, "173", "123456789012345", true, 0, false)
	f.Add(structuredNumber, "", false, "10", "00000000", true, -300, true)
	f.Add(structuredNumber, "", false, "", "3", true, -400, true)
	f.Add(structuredNumber, "", false, "17976931348623157", "", false, 292, true)

	f.Fuzz(func(t *testing.T, mode byte, text string, neg bool, intPart, fracPart string, hasFrac bool, exp int, hasExp bool) {
		if mode&1 == structuredNumber {
			intPart = onlyDigits(intPart, 40)
			fracPart = onlyDigits(fracPart, 40)
			if exp > 4000 {
				exp = 4000
			}
			if exp < -4000 {
				exp = -4000
			}
			text = composeNumberText(neg, intPart, fracPart, hasFrac, exp, hasExp)
			if len(text) > 90 {
				t.Skip()
			}
			checkFloatExactness(t, text)
			checkFloatDocumentViews(t, text)
			return
		}

		checkParse16DigitsText(t, text)
		if len(text) > 1<<12 {
			t.Skip()
		}
		src := []byte(text)
		trimmed := bytes.TrimSpace(src)
		if !validNumber(trimmed) {
			if _, err := parseFloat64(src); err == nil {
				t.Fatalf("parseFloat64 accepted %q, which is not a strict JSON number", clip(text))
			}
			var f64 float64
			if err := Unmarshal(src, &f64); err == nil && !bytes.Equal(trimmed, []byte("null")) {
				t.Fatalf("Unmarshal float64 accepted %q, which is not a strict JSON number", clip(text))
			}
			return
		}
		checkFloatExactness(t, string(trimmed))
		checkFloatDocumentViews(t, string(trimmed))
	})
}
