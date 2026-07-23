package slopjson

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/thesyncim/slopjson/document"
)

// ---------------------------------------------------------------------------
// accessor kind discipline. Wrong-kind accessors must report !ok with
// zero values on every API (Node, RawValue, Value).
// ---------------------------------------------------------------------------

func TestAccessorWrongKinds(t *testing.T) {
	src := []byte(`{"str":"12","num":42,"bool":true,"null":null,"arr":[1],"obj":{"a":1}}`)
	index := mustBuildIndex(t, src)
	value, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}

	check := func(name string, gotOK bool, zero bool) {
		t.Helper()
		if gotOK {
			t.Errorf("%s: ok = true on wrong kind", name)
		}
		if !zero {
			t.Errorf("%s: non-zero result on wrong kind", name)
		}
	}

	for _, field := range []string{"str", "num", "bool", "null", "arr", "obj"} {
		node, ok, err := index.Pointer("/" + field)
		if err != nil || !ok {
			t.Fatal(field, ok, err)
		}
		raw, ok, err := GetRaw(src, "/"+field)
		if err != nil || !ok {
			t.Fatal(field, ok, err)
		}
		val, ok := value.Get(field)
		if !ok {
			t.Fatal(field)
		}
		kind := node.Kind()
		if raw.Kind() != kind || val.Kind() != kind {
			t.Fatalf("%s: kinds disagree: node %v raw %v value %v", field, kind, raw.Kind(), val.Kind())
		}

		if kind != document.Number {
			n, ok := node.Int64()
			check(field+" Node.Int64", ok, n == 0)
			u, ok := node.Uint64()
			check(field+" Node.Uint64", ok, u == 0)
			f, ok := node.Float64()
			check(field+" Node.Float64", ok, f == 0)
			nb, ok := node.NumberBytes()
			check(field+" Node.NumberBytes", ok, nb == nil)
			rn, ok := raw.Int64()
			check(field+" RawValue.Int64", ok, rn == 0)
			ru, ok := raw.Uint64()
			check(field+" RawValue.Uint64", ok, ru == 0)
			rf, ok := raw.Float64()
			check(field+" RawValue.Float64", ok, rf == 0)
			vn, ok := val.Int64()
			check(field+" Value.Int64", ok, vn == 0)
			vu, ok := val.Uint64()
			check(field+" Value.Uint64", ok, vu == 0)
			vf, ok := val.Float64()
			check(field+" Value.Float64", ok, vf == 0)
		}
		if kind != document.Bool {
			b, ok := node.Bool()
			check(field+" Node.Bool", ok, !b)
			rb, ok := raw.Bool()
			check(field+" RawValue.Bool", ok, !rb)
			vb, ok := val.Bool()
			check(field+" Value.Bool", ok, !vb)
		}
		if kind != document.String {
			sb, ok := node.StringBytes()
			check(field+" Node.StringBytes", ok, sb == nil)
			rsb, ok := raw.StringBytes()
			check(field+" RawValue.StringBytes", ok, rsb == nil)
			dst, ok := node.AppendText([]byte("pre"))
			check(field+" Node.AppendText", ok, string(dst) == "pre")
			text, ok, err := raw.Text()
			if err != nil {
				t.Fatal(err)
			}
			check(field+" RawValue.Text", ok, text == "")
			vt, ok := val.Text()
			check(field+" Value.Text", ok, vt == "")
		}
		if kind != document.Array {
			n, ok := node.ArrayLen()
			check(field+" Node.ArrayLen", ok, n == 0)
			_, ok = node.Index(0)
			check(field+" Node.Index", ok, true)
			_, ok = val.Index(0)
			check(field+" Value.Index", ok, true)
			a, ok := val.Array()
			check(field+" Value.Array", ok, a == nil)
		}
		if kind != document.Object {
			n, ok := node.ObjectLen()
			check(field+" Node.ObjectLen", ok, n == 0)
			_, ok = node.Get("a")
			check(field+" Node.Get", ok, true)
			_, ok = val.Get("a")
			check(field+" Value.Get", ok, true)
			o, ok := val.Object()
			check(field+" Value.Object", ok, o == nil)
		}
		if kind != document.Null && node.IsNull() {
			t.Errorf("%s: Node.IsNull true", field)
		}
		if kind != document.Null && raw.IsNull() {
			t.Errorf("%s: RawValue.IsNull true", field)
		}
	}

	// Get on an empty object must be a clean miss (also exercises the
	// end-of-tape entry arithmetic under -race/checkptr).
	emptyIndex := mustBuildIndex(t, []byte(`{}`))
	if _, ok := emptyIndex.Root().Get("missing"); ok {
		t.Error("Get on empty object returned ok")
	}
	if _, ok := mustBuildIndex(t, []byte(`[]`)).Root().Index(0); ok {
		t.Error("Index on empty array returned ok")
	}
}

// ---------------------------------------------------------------------------
// number parsing must agree with strconv across all three accessor
// families, including overflow and exotic spellings.
// ---------------------------------------------------------------------------

func TestNumberAccessorConsistency(t *testing.T) {
	spellings := []string{
		"0", "-0", "9223372036854775807", "9223372036854775808",
		"-9223372036854775808", "-9223372036854775809",
		"18446744073709551615", "123456789012345678901234567890",
		"1e2", "1E+2", "1.0", "0.5", "-0.0", "1e999", "-1e999", "1e-999",
		"5e-324", "2.2250738585072011e-308", "9007199254740993",
		"1" + strings.Repeat("0", 400),
		"0." + strings.Repeat("0", 400) + "1",
	}
	for _, spelling := range spellings {
		src := []byte("[" + spelling + "]")

		wantInt, intErr := strconv.ParseInt(spelling, 10, 64)
		wantIntOK := intErr == nil
		wantUint, uintErr := strconv.ParseUint(spelling, 10, 64)
		wantUintOK := uintErr == nil
		wantFloat, floatErr := strconv.ParseFloat(spelling, 64)
		wantFloatOK := floatErr == nil

		node, ok, err := mustBuildIndex(t, src).Pointer("/0")
		if err != nil || !ok {
			t.Fatal(spelling, err)
		}
		raw, ok, err := GetRaw(src, "/0")
		if err != nil || !ok {
			t.Fatal(spelling, err)
		}
		val, ok, err := parseValuePointer(src, "/0")
		if err != nil || !ok {
			t.Fatal(spelling, err)
		}

		if text, ok := node.NumberText(); !ok || text != spelling {
			t.Errorf("%s: Node.NumberText = %q, %v", spelling, text, ok)
		}
		if text, ok := raw.NumberText(); !ok || text != spelling {
			t.Errorf("%s: RawValue.NumberText = %q, %v", spelling, text, ok)
		}
		if text, ok := val.NumberText(); !ok || text != spelling {
			t.Errorf("%s: Value.NumberText = %q, %v", spelling, text, ok)
		}

		for name, got := range map[string]func() (int64, bool){
			"Node.Int64":     node.Int64,
			"RawValue.Int64": raw.Int64,
			"Value.Int64":    val.Int64,
		} {
			n, ok := got()
			if ok != wantIntOK || (ok && n != wantInt) {
				t.Errorf("%s: %s = %d, %v; strconv = %d, %v", spelling, name, n, ok, wantInt, wantIntOK)
			}
		}
		for name, got := range map[string]func() (float64, bool){
			"Node.Float64":     node.Float64,
			"RawValue.Float64": raw.Float64,
			"Value.Float64":    val.Float64,
		} {
			f, ok := got()
			if ok != wantFloatOK || (ok && f != wantFloat) {
				t.Errorf("%s: %s = %g, %v; strconv = %g, %v", spelling, name, f, ok, wantFloat, wantFloatOK)
			}
		}
		for name, got := range map[string]func() (uint64, bool){
			"Node.Uint64":     node.Uint64,
			"RawValue.Uint64": raw.Uint64,
			"Value.Uint64":    val.Uint64,
		} {
			u, ok := got()
			if ok != wantUintOK || (ok && u != wantUint) {
				t.Errorf("%s: %s = %d, %v; strconv = %d, %v", spelling, name, u, ok, wantUint, wantUintOK)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// string decoding must be byte-exact with encoding/json for every
// escape form, on Node.AppendText, RawValue.Text, Value.Text, EachObject
// keys, and Node.Get key matching.
// ---------------------------------------------------------------------------

func TestStringDecodingVsStdlib(t *testing.T) {
	quoted := []string{
		`"plain"`,
		`""`,
		`"\u0041"`,
		`"\u0041B\u0043"`,
		`"\uD834\uDD1E"`,
		`"g\uD834\uDD1Eclef"`,
		`"\n\t\b\f\r\"\\\/"`,
		`"\u0000"`,
		`"a\u007Fb"`,
		`"\u00e9 raw and \u00E9 escaped"`,
		"\"\\u00e9 mixed raw \u00e9\"",
		`"\u2028\u2029"`,
		`"` + strings.Repeat(`\u00e9`, 300) + `"`,
		`"` + strings.Repeat(`ab\n`, 300) + `"`,
		`"ends with escape\\"`,
		"\"\uFFFD raw pass-through\"",
		`"\uFFFF"`,
	}
	for _, q := range quoted {
		var want string
		if err := json.Unmarshal([]byte(q), &want); err != nil {
			t.Fatalf("stdlib rejected %s: %v", q, err)
		}

		src := []byte("[" + q + "]")
		node, ok, err := mustBuildIndex(t, src).Pointer("/0")
		if err != nil || !ok {
			t.Fatal(q, err)
		}
		if got, ok := node.AppendText(nil); !ok || string(got) != want {
			t.Errorf("Node.AppendText(%s) = %q, %v; want %q", q, got, ok, want)
		}
		if got, ok := node.AppendText([]byte("prefix-")); !ok || string(got) != "prefix-"+want {
			t.Errorf("Node.AppendText(%s) with prefix = %q, %v", q, got, ok)
		}
		if sb, ok := node.StringBytes(); ok && string(sb) != want {
			t.Errorf("Node.StringBytes(%s) = %q, want %q", q, sb, want)
		}

		raw, ok, err := GetRaw(src, "/0")
		if err != nil || !ok {
			t.Fatal(q, err)
		}
		if got, ok, err := raw.Text(); err != nil || !ok || got != want {
			t.Errorf("RawValue.Text(%s) = %q, %v, %v; want %q", q, got, ok, err, want)
		}
		if got, ok, err := raw.AppendText([]byte("prefix-")); err != nil || !ok || string(got) != "prefix-"+want {
			t.Errorf("RawValue.AppendText(%s) = %q, %v, %v; want %q", q, got, ok, err, "prefix-"+want)
		}
		if got, clean := raw.StringBytes(); clean {
			if string(got) != want || strings.Contains(q, `\`) {
				t.Errorf("RawValue.StringBytes(%s) = %q, true; want clean alias only", q, got)
			}
		} else if !strings.Contains(q, `\`) {
			t.Errorf("RawValue.StringBytes(%s) = false for a clean string", q)
		}

		val, ok, err := parseValuePointer(src, "/0")
		if err != nil || !ok {
			t.Fatal(q, err)
		}
		if got, ok := val.Text(); !ok || got != want {
			t.Errorf("Value.Text(%s) = %q, %v; want %q", q, got, ok, want)
		}

		// The same content as an object key: EachObject key decoding, Node.Get,
		// and Value.Get key matching (tapeKeyEqual path).
		keyDoc := []byte("{" + q + ":1}")
		var gotKeys []string
		if err := EachObject(keyDoc, func(key string, _ RawValue) error {
			gotKeys = append(gotKeys, key)
			return nil
		}); err != nil {
			t.Fatalf("EachObject(%s): %v", keyDoc, err)
		}
		if len(gotKeys) != 1 || gotKeys[0] != want {
			t.Errorf("EachObject key for %s = %q, want %q", q, gotKeys, want)
		}
		if _, ok := mustBuildIndex(t, keyDoc).Root().Get(want); !ok {
			t.Errorf("Node.Get(%q) missed key spelled %s", want, q)
		}
		keyValue, err := Parse(keyDoc)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := keyValue.Get(want); !ok {
			t.Errorf("Value.Get(%q) missed key spelled %s", want, q)
		}
		// Near-miss keys must not match.
		if want != "" {
			miss := want[:len(want)-1]
			if _, ok := mustBuildIndex(t, keyDoc).Root().Get(miss); ok {
				t.Errorf("Node.Get(%q) matched key spelled %s", miss, q)
			}
			if _, ok := mustBuildIndex(t, keyDoc).Root().Get(want + "x"); ok {
				t.Errorf("Node.Get(%q) matched key spelled %s", want+"x", q)
			}
		}
	}
}

func TestRawValueAppendTextSteadyAllocs(t *testing.T) {
	raw := RawValue{src: []byte(`"` + strings.Repeat(`\u0061`, 256) + `"`)}
	dst := make([]byte, 0, len(raw.src))
	dst, ok, err := raw.AppendText(dst)
	if err != nil || !ok || len(dst) != 256 {
		t.Fatalf("AppendText warmup = len %d, %v, %v", len(dst), ok, err)
	}
	if n := testing.AllocsPerRun(100, func() {
		dst, ok, err = raw.AppendText(dst[:0])
	}); n != 0 {
		t.Fatalf("RawValue.AppendText allocated %.1f times with destination capacity", n)
	}
}
