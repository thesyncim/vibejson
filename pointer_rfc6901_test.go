package simdjson

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// RFC 6901 conformance across all six lookup paths, with values
// compared semantically. Uses the RFC's own example document plus adversarial
// keys: escaped-in-document unicode keys, digit keys on objects, "~01" order.
// ---------------------------------------------------------------------------

type pointerOutcome struct {
	ok    bool
	isErr bool
	raw   string // canonical JSON of target when ok
}

// parseValuePointer resolves a pointer through the dynamic tree. Other
// cross-API accessor contracts use it to compare status and errors.
func parseValuePointer(src []byte, pointer string) (Value, bool, error) {
	root, err := Parse(src)
	if err != nil {
		return Value{}, false, err
	}
	return root.Pointer(pointer)
}

// resolveAll runs one pointer through GetRaw, ScanFirstRaw, compiled GetRaw,
// Index.Pointer, Value.Pointer, and Value.PointerCompiled and asserts they
// agree; returns the outcome.
func resolveAll(t *testing.T, src []byte, pointer string) pointerOutcome {
	t.Helper()

	canonical := func(raw []byte) string {
		c, err := Canonicalize(raw)
		if err != nil {
			t.Fatalf("pointer %q target %q is not valid JSON: %v", pointer, raw, err)
		}
		return string(c)
	}

	getRaw, getOK, getErr := GetRaw(src, pointer)
	out := pointerOutcome{ok: getOK, isErr: getErr != nil}
	if getOK {
		out.raw = canonical(getRaw.Bytes())
	}

	scanRaw, scanOK, scanErr := ScanFirstRaw(src, pointer)
	if scanOK != getOK || (scanErr != nil) != (getErr != nil) || (scanOK && canonical(scanRaw.Bytes()) != out.raw) {
		t.Errorf("pointer %q: ScanFirstRaw = (%q, %v, %v), GetRaw = (%q, %v, %v)",
			pointer, scanRaw.Bytes(), scanOK, scanErr, getRaw.Bytes(), getOK, getErr)
	}

	compiled, compileErr := CompilePointer(pointer)
	if compileErr != nil {
		if getErr == nil {
			t.Errorf("pointer %q: CompilePointer rejected but GetRaw accepted", pointer)
		}
		return out
	}
	cRaw, cOK, cErr := compiled.GetRaw(src)
	if cOK != getOK || (cErr != nil) != (getErr != nil) || (cOK && canonical(cRaw.Bytes()) != out.raw) {
		t.Errorf("pointer %q: compiled GetRaw = (%q, %v, %v), dynamic = (%q, %v, %v)",
			pointer, cRaw.Bytes(), cOK, cErr, getRaw.Bytes(), getOK, getErr)
	}

	node, nodeOK, nodeErr := mustBuildIndex(t, src).Pointer(pointer)
	if nodeOK != getOK || (nodeErr != nil) != (getErr != nil) || (nodeOK && canonical(node.Raw().Bytes()) != out.raw) {
		t.Errorf("pointer %q: Index.Pointer = (%q, %v, %v), GetRaw = (%q, %v, %v)",
			pointer, node.Raw().Bytes(), nodeOK, nodeErr, getRaw.Bytes(), getOK, getErr)
	}

	root, parseErr := Parse(src)
	if parseErr != nil {
		t.Fatalf("Parse pointer fixture: %v", parseErr)
	}
	value, valueOK, valueErr := root.Pointer(pointer)
	if valueOK != getOK || (valueErr != nil) != (getErr != nil) {
		t.Errorf("pointer %q: Value.Pointer = (%v, %v), GetRaw = (%v, %v)", pointer, valueOK, valueErr, getOK, getErr)
	} else if valueOK {
		if got := canonical(value.AppendJSON(nil)); got != out.raw {
			t.Errorf("pointer %q: Value target = %s, raw target = %s", pointer, got, out.raw)
		}
	}
	compiledValue, compiledOK, compiledErr := root.PointerCompiled(compiled)
	if compiledOK != getOK || (compiledErr != nil) != (getErr != nil) {
		t.Errorf("pointer %q: Value.PointerCompiled = (%v, %v), GetRaw = (%v, %v)",
			pointer, compiledOK, compiledErr, getOK, getErr)
	} else if compiledOK {
		if got := canonical(compiledValue.AppendJSON(nil)); got != out.raw {
			t.Errorf("pointer %q: compiled Value target = %s, raw target = %s", pointer, got, out.raw)
		}
	}
	return out
}

func TestPointerRFC6901(t *testing.T) {
	rfcDoc := []byte(`{
		"foo": ["bar", "baz"],
		"": 0,
		"a/b": 1,
		"c%d": 2,
		"e^f": 3,
		"g|h": 4,
		"i\\j": 5,
		"k\"l": 6,
		" ": 7,
		"m~n": 8
	}`)
	for _, tc := range []struct {
		pointer string
		ok      bool
		isErr   bool
		raw     string
	}{
		{"", true, false, `{"":0," ":7,"a/b":1,"c%d":2,"e^f":3,"foo":["bar","baz"],"g|h":4,"i\\j":5,"k\"l":6,"m~n":8}`},
		{"/foo", true, false, `["bar","baz"]`},
		{"/foo/0", true, false, `"bar"`},
		{"/", true, false, `0`},
		{"/a~1b", true, false, `1`},
		{"/c%d", true, false, `2`},
		{"/e^f", true, false, `3`},
		{"/g|h", true, false, `4`},
		{"/i\\j", true, false, `5`},
		{"/k\"l", true, false, `6`},
		{"/ ", true, false, `7`},
		{"/m~0n", true, false, `8`},
		{"/foo/-", false, false, ``},                   // past-the-end: not found, no error
		{"/foo/2", false, false, ``},                   // out of range
		{"/foo/01", false, true, ``},                   // leading zero index must error
		{"/foo/00", false, true, ``},                   // ditto
		{"/foo/1e0", false, true, ``},                  // non-numeric index must error
		{"/foo/ 1", false, true, ``},                   // space is not numeric
		{"/foo/-1", false, true, ``},                   // negative index token is not numeric
		{"/foo/0/x", false, false, ``},                 // pointer into scalar: not found
		{"/nope", false, false, ``},                    // absent key
		{"/foo/99999999999999999999", false, true, ``}, // index overflows int
	} {
		got := resolveAll(t, rfcDoc, tc.pointer)
		if got.ok != tc.ok || got.isErr != tc.isErr || got.raw != tc.raw {
			t.Errorf("pointer %q = %+v, want ok=%v err=%v raw=%s", tc.pointer, got, tc.ok, tc.isErr, tc.raw)
		}
	}

	// "~01" decodes to the literal "~1", never to "/".
	tildeDoc := []byte(`{"~1":5,"~01":6,"/":7}`)
	if got := resolveAll(t, tildeDoc, "/~01"); !got.ok || got.raw != "5" {
		t.Errorf("/~01 = %+v, want the literal key %q", got, "~1")
	}
	if got := resolveAll(t, tildeDoc, "/~1"); !got.ok || got.raw != "7" {
		t.Errorf("/~1 = %+v, want the key %q", got, "/")
	}

	// Pure-digit and leading-zero keys are plain keys on objects.
	digitDoc := []byte(`{"0":10,"01":11,"-":12,"1e0":13}`)
	for pointer, want := range map[string]string{"/0": "10", "/01": "11", "/-": "12", "/1e0": "13"} {
		if got := resolveAll(t, digitDoc, pointer); !got.ok || got.raw != want {
			t.Errorf("object pointer %q = %+v, want %s", pointer, got, want)
		}
	}

	// Keys escaped in the DOCUMENT must match unescaped pointer tokens.
	escapedKeyDoc := []byte(`{"caf\u00e9":1,"tab\tkey":2,"g\uD834\uDD1Eclef":3,"a\/b":4,"nul\u0000key":5}`)
	for pointer, want := range map[string]string{
		"/caf\u00e9":       "1",
		"/tab\tkey":        "2",
		"/g\U0001D11Eclef": "3",
		"/a~1b":            "4",
		"/nul\x00key":      "5",
	} {
		if got := resolveAll(t, escapedKeyDoc, pointer); !got.ok || got.raw != want {
			t.Errorf("escaped-doc-key pointer %q = %+v, want %s", pointer, got, want)
		}
	}
	// Raw unicode key in the document, matched by the identical token.
	rawKeyDoc := []byte(`{"café":1,"𝄞":2}`)
	for pointer, want := range map[string]string{"/café": "1", "/𝄞": "2"} {
		if got := resolveAll(t, rawKeyDoc, pointer); !got.ok || got.raw != want {
			t.Errorf("raw-key pointer %q = %+v, want %s", pointer, got, want)
		}
	}

	// Empty keys, nested.
	emptyKeyDoc := []byte(`{"":{"":[5,6]}}`)
	if got := resolveAll(t, emptyKeyDoc, "//"); !got.ok || got.raw != "[5,6]" {
		t.Errorf("// = %+v, want [5,6]", got)
	}
	if got := resolveAll(t, emptyKeyDoc, "///1"); !got.ok || got.raw != "6" {
		t.Errorf("///1 = %+v, want 6", got)
	}

	// Very long token.
	longKey := strings.Repeat("k", 8192)
	longDoc := []byte(`{"` + longKey + `":42}`)
	if got := resolveAll(t, longDoc, "/"+longKey); !got.ok || got.raw != "42" {
		t.Errorf("long token = %+v, want 42", got)
	}

	// Pointer into scalar root.
	if got := resolveAll(t, []byte(`5`), "/a"); got.ok || got.isErr {
		t.Errorf("scalar root /a = %+v, want not-found without error", got)
	}
	if got := resolveAll(t, []byte(`"text"`), "/0"); got.ok || got.isErr {
		t.Errorf("string root /0 = %+v, want not-found without error", got)
	}
}
