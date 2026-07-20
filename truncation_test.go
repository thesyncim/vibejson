package simdjson

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// checkAPIAgreement asserts one input receives a consistent verdict from
// every entry point: the full validation/index/parse/transform consistency
// battery, the float64-boxing dynamic parser against encoding/json's
// range-rejection policy, and the typed decoder battery, which must reject
// anything invalid and never panic.
func checkAPIAgreement(t *testing.T, src []byte) bool {
	t.Helper()
	want := strictJSONValid(src)
	checkValidationConsistency(t, src, want)

	_, anyErr := unmarshalAnyForTest(src)
	if !want {
		if anyErr == nil {
			t.Fatalf("Unmarshal any accepted invalid input (length %d)", len(src))
		}
	} else {
		var std any
		stdErr := json.Unmarshal(src, &std)
		if (anyErr == nil) != (stdErr == nil) {
			t.Fatalf("Unmarshal any error = %v, encoding/json error = %v (length %d)", anyErr, stdErr, len(src))
		}
	}

	var typed benchDocument
	if err := Unmarshal(src, &typed); !want && err == nil {
		t.Fatalf("Unmarshal into struct accepted invalid input (length %d)", len(src))
	}
	// The any-bearing struct exercises the shared string arena: dynamic
	// values must retain their unescaped strings across later fields, so on
	// acceptance the result must match encoding/json exactly.
	type anyFieldProbe struct {
		B string `json:"b"`
		A any    `json:"a"`
		C string `json:"c"`
	}
	var probe anyFieldProbe
	if err := Unmarshal(src, &probe); !want && err == nil {
		t.Fatalf("Unmarshal into any-bearing struct accepted invalid input (length %d)", len(src))
	} else if err == nil {
		var std anyFieldProbe
		if json.Unmarshal(src, &std) == nil && !reflect.DeepEqual(probe, std) {
			t.Fatalf("any-bearing decode differs from encoding/json: %+v vs %+v", probe, std)
		}
	}
	var tree map[string]any
	if err := Unmarshal(src, &tree); !want && err == nil {
		t.Fatalf("Unmarshal into map accepted invalid input (length %d)", len(src))
	}
	var list []any
	if err := Unmarshal(src, &list); !want && err == nil {
		t.Fatalf("Unmarshal into slice accepted invalid input (length %d)", len(src))
	}

	// FieldCursor must resolve every key of every object in the document exactly
	// like the independent first-forward-match reference, including nested,
	// escaped, duplicate, and empty objects. Only reachable on acceptance.
	if want {
		if v, err := Parse(src); err == nil {
			checkFieldCursorTree(t, v)
		}
	}
	return want
}

// checkFieldCursorTree walks every object and array in v and asserts the field
// cursor agrees with the reference oracle on that object, then recurses. This
// folds the cursor into the truncation, mutation, and fuzz sweeps so it is
// checked against the whole adversarial corpus at every nesting level.
func checkFieldCursorTree(t *testing.T, v Value) {
	t.Helper()
	switch v.Kind() {
	case Object:
		members, ok := v.Object()
		if !ok {
			t.Fatal("Object() failed on Object kind")
		}
		ref := make([]refMember, len(members))
		var order []string
		seen := map[string]bool{}
		for i, m := range members {
			ref[i] = refMember{key: m.Key, raw: append([]byte(nil), m.Value.Node().Raw().Bytes()...)}
			if !seen[m.Key] {
				seen[m.Key] = true
				order = append(order, m.Key)
			}
		}
		// Look up every distinct key twice (checking wrap-resume) plus one
		// guaranteed-absent key, against the reference cursor.
		lookups := make([]string, 0, len(order)*2+1)
		for _, k := range order {
			lookups = append(lookups, k, k)
		}
		lookups = append(lookups, "\x00__no_such_key__")
		rc := &refCursor{members: ref}
		fc := v.Fields()
		for _, key := range lookups {
			wantRaw, wantOK := rc.find(key)
			got, gotOK := fc.Find(key)
			if gotOK != wantOK {
				t.Fatalf("FieldCursor key=%q ok=%v want %v", key, gotOK, wantOK)
			}
			if gotOK && !bytes.Equal(got.Node().Raw().Bytes(), wantRaw) {
				t.Fatalf("FieldCursor key=%q raw=%q want %q", key, got.Node().Raw().Bytes(), wantRaw)
			}
		}
		for _, m := range members {
			checkFieldCursorTree(t, m.Value)
		}
	case Array:
		els, ok := v.Array()
		if !ok {
			t.Fatal("Array() failed on Array kind")
		}
		for _, el := range els {
			checkFieldCursorTree(t, el)
		}
	}
}

// truncationTortureDocs are generated documents whose prefixes cut through
// the scanners' hardest states: container depth at the limit, every escape
// form, multi-byte characters, and boundary numbers.
func truncationTortureDocs() []struct {
	name string
	doc  []byte
} {
	return []struct {
		name string
		doc  []byte
	}{
		{
			"depth limit",
			[]byte(strings.Repeat("[", defaultMaxDepth) + "0" + strings.Repeat("]", defaultMaxDepth)),
		},
		{
			"escapes and multibyte",
			[]byte(`{"escapes":"A𝄞\n\t\\\" and \/ \b\f\r","text":"é日本語𝄞 plain","empty":""}`),
		},
		{
			"boundary numbers",
			[]byte(`[0,-0,0.0,1e308,5e-324,2.2250738585072011e-308,9007199254740993,` +
				`1.5e+9999,123e-10000000,-123.4567890123456789e-12,1234567890123456.7890123456789012]`),
		},
		{
			"literals and structure",
			[]byte(`[true,false,null,{"a":[]},{"b":{}},[[[["deep"]]]],"tail"]`),
		},
	}
}

// TestTruncationSweep validates every prefix of every small JSONTestSuite
// document and of the torture documents, and a sampled set of prefixes of
// large documents, through the whole API surface.
func TestTruncationSweep(t *testing.T) {
	t.Parallel() // pure differential: reads fixtures, decodes into locals, no globals
	entries, err := os.ReadDir(jsonTestSuiteDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 2<<10 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jsonTestSuiteDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sweepPrefixes(t, data)
	}

	for _, torture := range truncationTortureDocs() {
		t.Run(torture.name, func(t *testing.T) {
			sweepPrefixes(t, torture.doc)
		})
	}

	t.Run("large indented", func(t *testing.T) {
		indented, err := Indent(benchRecordsJSON(512), "", "    ")
		if err != nil {
			t.Fatal(err)
		}
		sweepPrefixes(t, indented)
	})
}

// sweepPrefixes checks every prefix of small documents; for large ones it
// checks the head and tail densely and strides through the middle.
func sweepPrefixes(t *testing.T, doc []byte) {
	t.Helper()
	if testing.Short() && len(doc) > 128 {
		// Instrumented runs retain dense head/tail coverage and distribute 16
		// cuts through the middle. Normal runs still check every small prefix.
		const edge = 16
		for i := 0; i <= edge; i++ {
			checkAPIAgreement(t, doc[:i])
		}
		stride := max(1, (len(doc)-2*edge)/16)
		for i := edge + 1; i < len(doc)-edge; i += stride {
			checkAPIAgreement(t, doc[:i])
		}
		for i := len(doc) - edge; i <= len(doc); i++ {
			checkAPIAgreement(t, doc[:i])
		}
		return
	}
	if len(doc) <= 2<<10 {
		for i := 0; i <= len(doc); i++ {
			checkAPIAgreement(t, doc[:i])
		}
		return
	}
	stride := 197
	if len(doc) > 32<<10 {
		stride = 613
	}
	for i := 0; i <= 256; i++ {
		checkAPIAgreement(t, doc[:i])
	}
	for i := 256; i < len(doc)-256; i += stride {
		checkAPIAgreement(t, doc[:i])
	}
	for i := len(doc) - 256; i <= len(doc); i++ {
		checkAPIAgreement(t, doc[:i])
	}
}

// hostileMutationAlphabet holds the bytes most likely to flip a scanner into
// a wrong state: structural characters, escape and literal starters, number
// syntax, controls, and UTF-8 lead and continuation bytes.
var hostileMutationAlphabet = []byte{
	'"', '\\', '{', '}', '[', ']', ':', ',',
	'0', '9', 'x', 'e', 'E', 't', 'f', 'n', '.', '+', '-',
	' ', '\t', '\n', '\r',
	0x00, 0x1F, 0x7F, 0x80, 0xC2, 0xE2, 0xED, 0xF4, 0xFF,
}

// TestMutationSweep applies every hostile byte at every position of a
// benchmark-shaped document, plus every single-byte deletion, and checks the
// full API agreement on each mutant.
func TestMutationSweep(t *testing.T) {
	t.Parallel() // pure differential: mutates a local buffer, decodes into locals
	doc := benchRecordsJSON(8)
	if testing.Short() {
		doc = benchRecordsJSON(2)
	}
	mutant := make([]byte, len(doc))
	positionStride := 1
	if testing.Short() {
		positionStride = 8
	}
	for i := 0; i < len(doc); i += positionStride {
		for _, b := range hostileMutationAlphabet {
			if doc[i] == b {
				continue
			}
			copy(mutant, doc)
			mutant[i] = b
			checkAPIAgreement(t, mutant)
		}
	}

	deleted := make([]byte, 0, len(doc))
	for i := 0; i < len(doc); i += positionStride {
		deleted = append(append(deleted[:0], doc[:i]...), doc[i+1:]...)
		checkAPIAgreement(t, deleted)
	}
}

// addAPIConsistencySeeds preserves the arbitrary-byte API campaign's corpus
// inside FuzzDecodeTrust, which runs the same strict and stdlib oracles.
func addAPIConsistencySeeds(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		[]byte(`null`),
		[]byte(`true`),
		[]byte(`0`),
		[]byte(`-0`),
		[]byte(`-0.125e+9`),
		[]byte(`-12.34e+56`),
		[]byte(`1e`),
		[]byte(`1234567890123456.25`),
		[]byte(`[1234567890123456,1000000000000001,-1000000000000002]`),
		[]byte(`1.00000000000000011102230246251565404236316680908203125`),
		[]byte(`""`),
		[]byte(`"plain"`),
		[]byte(`"hello\nworld"`),
		[]byte(`"\uD834\uDD1E"`),
		[]byte(`{"a":[1,true,null],"b":{"c":"d"}}`),
		[]byte(`{"a":[1,true,null,"text"],"b":2.5}`),
		[]byte(`[[]]`),
		[]byte(`{"a":[]}`),
		[]byte(`01`),
		[]byte(`[1,]`),
		[]byte(`{"a":}`),
		[]byte(`"\uD800"`),
		{'"', 0xff, '"'},
	} {
		f.Add(seed)
	}
	addJSONTestSuiteSeeds(f)
	for _, torture := range truncationTortureDocs() {
		f.Add(torture.doc)
		f.Add(torture.doc[:len(torture.doc)/2])
	}
	f.Add(benchRecordsJSON(2))
	f.Add([]byte(`{"b":"p` + jsonUnicodeEscape("0042") + `q","a":"x` + jsonUnicodeEscape("0041") + `y","c":"r` + jsonUnicodeEscape("0043") + `s"}`))
}

func checkUnmarshalAnyValueParity(t *testing.T, src []byte) {
	t.Helper()
	var want any
	wantErr := json.Unmarshal(src, &want)
	for _, parse := range []func([]byte) (any, error){unmarshalAnyForTest, decodeAnyZeroCopyForTest} {
		got, gotErr := parse(src)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("parse error = %v, encoding/json error = %v", gotErr, wantErr)
		}
		if gotErr == nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("parse = %#v, encoding/json = %#v", got, want)
		}
	}
}
