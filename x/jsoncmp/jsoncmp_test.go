package jsoncmp

import (
	"testing"

	"github.com/thesyncim/vibejson/document"
)

func TestNumberEqual(t *testing.T) {
	equal := [][2]string{
		{"0", "0"},
		{"0", "-0"},
		{"0", "0.0"},
		{"-0.0", "0e10"},
		{"0e-999999999999999999999", "-0.000e+5"},
		{"100", "1e2"},
		{"100", "1.00e+2"},
		{"0.01", "1e-2"},
		{"-12.3400", "-1.234e1"},
		{"123456789012345678901234567890", "1.2345678901234567890123456789e29"},
		{"1e100000000000000000000", "10e99999999999999999999"},
		{"0.1e-99999999999999999999", "1e-100000000000000000000"},
		{"0.00012345", "1.2345e-4"},
		{"0.000000000000000000001", "1e-21"},
		{"9999999999999999999", "9.999999999999999999e18"},
		{"18446744073709551615", "1.8446744073709551615e19"},
		{"18446744073709551616", "1.8446744073709551616e19"},
	}
	for _, pair := range equal {
		if !NumberEqual([]byte(pair[0]), []byte(pair[1])) {
			t.Errorf("NumberEqual(%q, %q) = false, want true", pair[0], pair[1])
		}
		if !NumberEqual([]byte(pair[1]), []byte(pair[0])) {
			t.Errorf("NumberEqual(%q, %q) = false, want true (symmetric)", pair[1], pair[0])
		}
	}

	notEqual := [][2]string{
		{"", ""},
		{"", "0"},
		{"1", "-1"},
		{"100", "101"},
		{"0.1", "0.10000000000000001"},
		{"1e2", "1e3"},
		{"123456789012345678901234567890", "123456789012345678901234567891"},
		{"1e100000000000000000000", "1e100000000000000000001"},
		{"-", "0"},
		{"1.", "1"},
		{"1e", "1"},
		{"1e+", "1"},
	}
	for _, pair := range notEqual {
		if NumberEqual([]byte(pair[0]), []byte(pair[1])) {
			t.Errorf("NumberEqual(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
}

func TestStringByteIter(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"hello", "hello"},
		{`a\nb\tc\rd\be\ff\"g\\h\/i`, "a\nb\tc\rd\be\ff\"g\\h/i"},
		{`caf\u00e9`, "café"},
		{`caf\u00E9`, "café"},
		{`\uD83D\uDE00`, "😀"},
		{`hi \ud83d\ude00!`, "hi 😀!"},
	}
	for _, tc := range cases {
		it := StringByteIter{Raw: []byte(tc.raw)}
		var got []byte
		for {
			b, ok := it.Next()
			if !ok {
				break
			}
			got = append(got, b)
		}
		if string(got) != tc.want {
			t.Errorf("StringByteIter(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	// Truncated escapes must terminate cleanly without panicking.
	for _, bad := range []string{`\`, `\u`, `\u12`, `\uZZZZ`} {
		it := StringByteIter{Raw: []byte(bad)}
		for _, ok := it.Next(); ok; _, ok = it.Next() {
		}
	}
}

func TestRawStringEqual(t *testing.T) {
	const esc = document.TapeFlagEscaped

	equal := []struct {
		a      string
		aFlags uint8
		b      string
		bFlags uint8
	}{
		{`""`, 0, `""`, 0},
		{`"hello"`, 0, `"hello"`, 0},
		{`"café"`, 0, `"caf\u00e9"`, esc},
		{`"caf\u00e9"`, esc, `"café"`, 0},
		{`"caf\u00e9"`, esc, `"caf\u00E9"`, esc},
		{`"\uD83D\uDE00"`, esc, `"😀"`, 0},
		{`"\ud83d\ude00"`, esc, `"\uD83D\uDE00"`, esc},
		{`"a\nb"`, esc, `"a\u000ab"`, esc},
	}
	for _, tc := range equal {
		if !RawStringEqual([]byte(tc.a), tc.aFlags, []byte(tc.b), tc.bFlags) {
			t.Errorf("RawStringEqual(%s, %d, %s, %d) = false, want true", tc.a, tc.aFlags, tc.b, tc.bFlags)
		}
		if !RawStringEqual([]byte(tc.a), esc, []byte(tc.b), esc) {
			t.Errorf("RawStringEqual(%s, esc, %s, esc) = false, want true", tc.a, tc.b)
		}
	}

	notEqual := []struct {
		a      string
		aFlags uint8
		b      string
		bFlags uint8
	}{
		{`"a"`, 0, `"b"`, 0},
		{`"a"`, 0, `"ab"`, 0},
		{`"caf\u00e9"`, esc, `"cafe"`, 0},
		{`"\uD83D\uDE00"`, esc, `"\uD83D\uDE01"`, esc},
		{`""`, 0, `""`[:1], 0},
	}
	for _, tc := range notEqual {
		if RawStringEqual([]byte(tc.a), tc.aFlags, []byte(tc.b), tc.bFlags) {
			t.Errorf("RawStringEqual(%s, %d, %s, %d) = true, want false", tc.a, tc.aFlags, tc.b, tc.bFlags)
		}
	}
}

func TestRawStringEqualString(t *testing.T) {
	const esc = document.TapeFlagEscaped

	if !RawStringEqualString([]byte(`"hello"`), 0, "hello") {
		t.Fatal("unescaped match failed")
	}
	if !RawStringEqualString([]byte(`"caf\u00e9"`), esc, "café") {
		t.Fatal("escaped unicode match failed")
	}
	if !RawStringEqualString([]byte(`"\uD83D\uDE00"`), esc, "😀") {
		t.Fatal("surrogate pair match failed")
	}
	if RawStringEqualString([]byte(`"caf\u00e9"`), esc, "caf") {
		t.Fatal("prefix matched full string")
	}
	if RawStringEqualString([]byte(`"`), esc, "") {
		t.Fatal("short slice matched")
	}
}

func TestZeroAlloc(t *testing.T) {
	aNum := []byte("123456789012345678901234567890")
	bNum := []byte("1.2345678901234567890123456789e29")
	if allocs := testing.AllocsPerRun(100, func() {
		if !NumberEqual(aNum, bNum) {
			t.Fatal("unexpected mismatch")
		}
	}); allocs != 0 {
		t.Fatalf("NumberEqual allocated %.1f times", allocs)
	}

	aStr := []byte(`"caf\u00e9 \uD83D\uDE00"`)
	bStr := []byte(`"caf\u00E9 \ud83d\ude00"`)
	if allocs := testing.AllocsPerRun(100, func() {
		if !RawStringEqual(aStr, document.TapeFlagEscaped, bStr, document.TapeFlagEscaped) {
			t.Fatal("unexpected mismatch")
		}
	}); allocs != 0 {
		t.Fatalf("RawStringEqual allocated %.1f times", allocs)
	}

	if allocs := testing.AllocsPerRun(100, func() {
		if !RawStringEqualString(aStr, document.TapeFlagEscaped, "café 😀") {
			t.Fatal("unexpected mismatch")
		}
	}); allocs != 0 {
		t.Fatalf("RawStringEqualString allocated %.1f times", allocs)
	}
}
