package slopjson

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// depth-limit agreement between Valid and the seeker-based APIs.
// ---------------------------------------------------------------------------

func TestDepthLimitAgreement(t *testing.T) {
	deepArray := func(depth int) []byte {
		return []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
	}
	for _, depth := range []int{defaultMaxDepth, defaultMaxDepth + 1} {
		src := deepArray(depth)
		want := Valid(src)

		if err := EachArray(src, nil); (err == nil) != want {
			t.Errorf("depth %d: EachArray err = %v, Valid = %v", depth, err, want)
		}
		if _, _, err := GetRaw(src, "/0"); (err == nil) != want {
			t.Errorf("depth %d: GetRaw err = %v, Valid = %v", depth, err, want)
		}
		if _, _, err := ScanFirstRaw(src, "/0"); (err == nil) != want {
			t.Errorf("depth %d: ScanFirstRaw err = %v, Valid = %v", depth, err, want)
		}
		if _, err := AppendCompact(nil, src); (err == nil) != want {
			t.Errorf("depth %d: AppendCompact err = %v, Valid = %v", depth, err, want)
		}
		if _, err := unmarshalAnyForTest(src); (err == nil) != want {
			t.Errorf("depth %d: Unmarshal any err = %v, Valid = %v", depth, err, want)
		}
		if _, err := Indent(src, "", " "); (err == nil) != want {
			t.Errorf("depth %d: Indent err = %v, Valid = %v", depth, err, want)
		}
		if _, err := Canonicalize(src); (err == nil) != want {
			t.Errorf("depth %d: Canonicalize err = %v, Valid = %v", depth, err, want)
		}
	}

	// Nested objects too.
	deepObject := strings.Repeat(`{"k":`, defaultMaxDepth-1) + "0" + strings.Repeat("}", defaultMaxDepth-1)
	if !Valid([]byte(deepObject)) {
		t.Fatal("deep object should be valid")
	}
	if err := EachObject([]byte(deepObject), nil); err != nil {
		t.Errorf("EachObject rejected a Valid deep object: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Canonicalize contract. Doc: "sorts object members recursively and
// emits compact JSON." Verify the observable contract precisely.
// ---------------------------------------------------------------------------

func TestCanonicalizeContract(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
	}{
		// Number spellings are preserved verbatim (no RFC 8785 normalization is
		// promised or performed).
		{"numbers", `{"a":1e2,"b":-0,"c":1.50,"d":100}`, `{"a":1e2,"b":-0,"c":1.50,"d":100}`},
		// Duplicates: both retained, stable among equals, sorted by key.
		{"duplicates", `{"b":1,"a":9,"a":8}`, `{"a":9,"a":8,"b":1}`},
		// Keys compare after unescaping; output re-escapes minimally.
		{"escaped keys", `{"\u0062":1,"a":2}`, `{"a":2,"b":1}`},
		// Nested objects sorted recursively; arrays keep order.
		{"nested", `{"z":{"b":1,"a":2},"y":[{"d":1,"c":2}]}`, `{"y":[{"c":2,"d":1}],"z":{"a":2,"b":1}}`},
		// Byte-order (Go string <) key sorting.
		{"ascii order", `{"Z":1,"a":2,"0":3,"~":4}`, `{"0":3,"Z":1,"a":2,"~":4}`},
	} {
		got, err := Canonicalize([]byte(tc.src))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: Canonicalize(%s) = %s, want %s", tc.name, tc.src, got, tc.want)
		}
	}

	// Record the non-ASCII ordering rule: byte order, not UTF-16 code-unit
	// order. U+FB00 (EF AC 80) sorts before U+1F600 (F0 9F 98 80) by bytes;
	// RFC 8785 would sort U+1F600 (D83D DE00) before U+FB00.
	got, err := Canonicalize([]byte(`{"ﬀ":1,"😀":2}`))
	if err != nil {
		t.Fatal(err)
	}
	byteOrder := `{"ﬀ":1,"😀":2}`
	utf16Order := `{"😀":2,"ﬀ":1}`
	switch string(got) {
	case byteOrder:
		t.Logf("Canonicalize sorts keys in byte order (not RFC 8785 UTF-16 order): %s", got)
	case utf16Order:
		t.Logf("Canonicalize sorts keys in UTF-16 code-unit order: %s", got)
	default:
		t.Errorf("Canonicalize non-ASCII order = %s", got)
	}
}

// ---------------------------------------------------------------------------
// Indent with adversarial indents and content. Whitespace indents
// must produce valid JSON that compacts back to the same document; the U+2028
// case must stay valid.
// ---------------------------------------------------------------------------

func TestIndentContract(t *testing.T) {
	src := []byte(`{"a":[1,{"b":"x"},[]],"c":{},"d":"e f","g":"h` + " " + `i"}`)
	wantCompactCanonical, err := Canonicalize(src)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ prefix, indent string }{
		{"", "  "},
		{"", "\t"},
		{"", ""},
		{"\t\t", " "},
		{"", " \t "},
	} {
		pretty, err := Indent(src, tc.prefix, tc.indent)
		if err != nil {
			t.Fatalf("prefix=%q indent=%q: %v", tc.prefix, tc.indent, err)
		}
		if !Valid(pretty) {
			t.Errorf("prefix=%q indent=%q: output is not valid JSON: %s", tc.prefix, tc.indent, pretty)
		}
		roundTrip, err := Canonicalize(pretty)
		if err != nil {
			t.Fatalf("prefix=%q indent=%q: %v", tc.prefix, tc.indent, err)
		}
		if !bytes.Equal(roundTrip, wantCompactCanonical) {
			t.Errorf("prefix=%q indent=%q: semantic change:\n%s\n%s", tc.prefix, tc.indent, roundTrip, wantCompactCanonical)
		}
	}

	// U+2028/U+2029 inside strings, both raw and escaped in the source: the
	// indented output must stay valid JSON and preserve the string value.
	for _, doc := range []string{
		"[\"a b c\"]",
		`["a\u2028b\u2029c"]`,
	} {
		pretty, err := Indent([]byte(doc), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if !Valid(pretty) {
			t.Errorf("Indent(%q) output invalid: %q", doc, pretty)
		}
		var got []string
		if err := json.Unmarshal(pretty, &got); err != nil || len(got) != 1 || got[0] != "a b c" {
			t.Errorf("Indent(%q) round-trip = %q, %v", doc, got, err)
		}
	}

	// Non-whitespace indent: match stdlib's behavior class (stdlib also emits
	// invalid JSON in that case, so just record parity).
	ours, err := Indent([]byte(`[1,2]`), "", "→")
	if err != nil {
		t.Fatal(err)
	}
	var stdBuf bytes.Buffer
	if err := json.Indent(&stdBuf, []byte(`[1,2]`), "", "→"); err != nil {
		t.Fatal(err)
	}
	if Valid(ours) != json.Valid(stdBuf.Bytes()) {
		t.Errorf("unicode indent validity: ours %v (%s), stdlib %v (%s)", Valid(ours), ours, json.Valid(stdBuf.Bytes()), stdBuf.Bytes())
	}
}

// TestIndentPreservesEscapeSpelling pins Indent to json.Indent byte for
// byte: like stdlib, it copies string and number tokens from the source
// verbatim rather than re-encoding a decoded value, so \uXXXX, \/, and
// surrogate-pair spellings survive. Inputs are pre-compacted so no surrounding
// whitespace is involved.
func TestIndentPreservesEscapeSpelling(t *testing.T) {
	for _, src := range []string{
		`{"k":"A\/>"}`,
		`["😀","plain"]`,
		`{"num":1e10,"big":123456789012345678,"neg":-0.5}`,
		`{"nested":{"a":"\t\n","b":[true,null,false]}}`,
		`"  "`,
	} {
		got, err := Indent([]byte(src), "", "  ")
		if err != nil {
			t.Fatalf("Indent(%s): %v", src, err)
		}
		var want bytes.Buffer
		if err := json.Indent(&want, []byte(src), "", "  "); err != nil {
			t.Fatalf("json.Indent(%s): %v", src, err)
		}
		if string(got) != want.String() {
			t.Errorf("Indent(%s) not byte-identical to json.Indent:\n simd=%q\n json=%q", src, got, want.String())
		}
	}
}
