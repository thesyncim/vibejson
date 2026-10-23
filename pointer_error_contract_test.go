package vibejson

import (
	"errors"
	"testing"

	"github.com/thesyncim/vibejson/document"
)

func TestPointerValidatesSyntaxAfterAbsentTarget(t *testing.T) {
	src := []byte(`{"a":1,"list":[0]}`)
	value, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	index := mustBuildIndex(t, src)
	for _, pointer := range []string{
		"/missing/~2", "/missing/~", "/a/deeper/~2",
		"/list/9/~2", "/list/-/~2",
		"/missing/valid", "/a/deeper/valid", "/list/9/valid", "/list/-/valid",
	} {
		t.Run(pointer, func(t *testing.T) {
			_, compileErr := CompilePointer(pointer)
			_, rawOK, rawErr := GetRaw(src, pointer)
			_, nodeOK, nodeErr := index.Pointer(pointer)
			_, valueOK, valueErr := value.Pointer(pointer)
			_, zeroOK, zeroErr := (Node{}).Pointer(pointer)
			for _, got := range []struct {
				name string
				ok   bool
				err  error
			}{
				{"raw", rawOK, rawErr}, {"node", nodeOK, nodeErr},
				{"value", valueOK, valueErr}, {"zero node", zeroOK, zeroErr},
			} {
				var pointerErr *document.PointerError
				if got.ok || (got.err != nil) != (compileErr != nil) ||
					(got.err != nil && !errors.As(got.err, &pointerErr)) {
					t.Errorf("%s = ok %v, err %v; CompilePointer error = %v", got.name, got.ok, got.err, compileErr)
				}
			}
		})
	}
}

func TestPointerLastDuplicateReplacesLookupError(t *testing.T) {
	for _, tc := range []struct {
		src        string
		pointer    string
		want       string
		pointerErr bool
	}{
		{`{"dup":[],"dup":{"x":1}}`, "/dup/x", "1", false},
		{`{"dup":[],"dup":{"01":2}}`, "/dup/01", "2", false},
		{`{"dup":[],"dup":{},"dup":{"x":3}}`, "/dup/x", "3", false},
		{`{"dup":[],"dup":{}}`, "/dup/x", "", false},
		{`{"dup":[],"dup":0}`, "/dup/x", "", false},
		{`{"dup":{"inner":[]},"dup":{"inner":{"x":3}}}`, "/dup/inner/x", "3", false},
		{`{"dup":[[]],"dup":[{"x":4}]}`, "/dup/0/x", "4", false},
		{`{"dup":[[],7,{"a":0}],"dup":[{"x":4}]}`, "/dup/0/x", "4", false},
		{`{"d\u0075p":[],"dup":{"x":5}}`, "/dup/x", "5", false},
		{`{"dup":{"x":1},"dup":[]}`, "/dup/x", "", true},
		{`{"dup":[],"other":{"x":1}}`, "/dup/x", "", true},
		{`{"dup":{"inner":[]}}`, "/dup/inner/x", "", true},
	} {
		t.Run(tc.src+tc.pointer, func(t *testing.T) {
			src := []byte(tc.src)
			compiled := MustCompilePointer(tc.pointer)
			value, err := Parse(src)
			if err != nil {
				t.Fatal(err)
			}
			index := mustBuildIndex(t, src)
			raw, rawOK, rawErr := GetRaw(src, tc.pointer)
			compiledRaw, compiledOK, compiledErr := compiled.GetRaw(src)
			trustedRaw, trustedOK, trustedErr := compiled.GetRawTrusted(src)
			node, nodeOK, nodeErr := index.Pointer(tc.pointer)
			val, valueOK, valueErr := value.Pointer(tc.pointer)
			valueRaw := ""
			if valueOK {
				valueRaw = string(val.AppendJSON(nil))
			}
			for _, got := range []struct {
				name string
				raw  string
				ok   bool
				err  error
			}{
				{"raw", string(raw.Bytes()), rawOK, rawErr},
				{"compiled raw", string(compiledRaw.Bytes()), compiledOK, compiledErr},
				{"trusted raw", string(trustedRaw.Bytes()), trustedOK, trustedErr},
				{"node", string(node.Raw().Bytes()), nodeOK, nodeErr},
				{"value", valueRaw, valueOK, valueErr},
			} {
				var pointerErr *document.PointerError
				if (got.err != nil) != tc.pointerErr ||
					(tc.pointerErr && !errors.As(got.err, &pointerErr)) ||
					got.ok != (tc.want != "") || got.raw != tc.want {
					t.Errorf("%s = raw %q, ok %v, err %v; want raw %q, pointer error %v", got.name, got.raw, got.ok, got.err, tc.want, tc.pointerErr)
				}
			}
		})
	}
}

func TestPointerScanFirstPreservesLookupError(t *testing.T) {
	compiled := MustCompilePointer("/dup/x")
	for _, tc := range []struct {
		src        string
		want       string
		pointerErr bool
	}{
		{`{"dup":[],"dup":{"x":1}}`, "", true},
		{`{"dup":{"x":1},"dup":[]}`, "1", false},
	} {
		t.Run(tc.src, func(t *testing.T) {
			src := []byte(tc.src)
			for _, lookup := range []struct {
				name string
				get  func([]byte) (RawValue, bool, error)
			}{
				{"dynamic", func(src []byte) (RawValue, bool, error) { return ScanFirstRaw(src, compiled.String()) }},
				{"compiled", compiled.ScanFirstRaw},
				{"trusted", compiled.ScanFirstRawTrusted},
			} {
				raw, ok, err := lookup.get(src)
				var pointerErr *document.PointerError
				if (err != nil) != tc.pointerErr || (tc.pointerErr && !errors.As(err, &pointerErr)) ||
					ok != (tc.want != "") || string(raw.Bytes()) != tc.want {
					t.Errorf("%s = raw %q, ok %v, err %v; want raw %q, pointer error %v", lookup.name, raw.Bytes(), ok, err, tc.want, tc.pointerErr)
				}
			}
		})
	}
}

func TestPointerOverwrittenLookupErrorStillValidatesSource(t *testing.T) {
	for _, tc := range []struct {
		src      string
		pointer  string
		maxDepth int
	}{
		{`{"dup":[bad],"dup":{"x":1}}`, "/dup/x", 0},
		{`{"dup":[1,],"dup":{"x":1}}`, "/dup/x", 0},
		{`{"dup":{"inner":[bad]},"dup":{"inner":{"x":1}}}`, "/dup/inner/x", 0},
		{`{"dup":[[],bad],"dup":[{"x":1}]}`, "/dup/0/x", 0},
		{`{"dup":[],"other":[bad],"dup":{"x":1}}`, "/dup/x", 0},
		{`{"dup":[],"dup":{"x":1}} false`, "/dup/x", 0},
		{`{"dup":[[]],"dup":{"x":1}}`, "/dup/x", 2},
	} {
		t.Run(tc.src, func(t *testing.T) {
			src := []byte(tc.src)
			compiled := MustCompilePointer(tc.pointer)
			opts := Options{MaxDepth: tc.maxDepth}
			_, dynamicOK, dynamicErr := GetRawOptions(src, tc.pointer, opts)
			_, compiledOK, compiledErr := compiled.GetRawOptions(src, opts)
			for _, got := range []struct {
				name string
				ok   bool
				err  error
			}{{"dynamic", dynamicOK, dynamicErr}, {"compiled", compiledOK, compiledErr}} {
				var syntaxErr *SyntaxError
				if got.ok || !errors.As(got.err, &syntaxErr) {
					t.Errorf("%s = ok %v, err %v; want SyntaxError", got.name, got.ok, got.err)
				}
			}
		})
	}
}

func TestPointerLastDuplicateAtDepthLimit(t *testing.T) {
	src := []byte(`{"dup":[],"dup":{"x":1}}`)
	compiled := MustCompilePointer("/dup/x")
	opts := Options{MaxDepth: 2}
	for _, lookup := range []struct {
		name string
		get  func() (RawValue, bool, error)
	}{
		{"dynamic", func() (RawValue, bool, error) { return GetRawOptions(src, compiled.String(), opts) }},
		{"compiled", func() (RawValue, bool, error) { return compiled.GetRawOptions(src, opts) }},
	} {
		raw, ok, err := lookup.get()
		if err != nil || !ok || string(raw.Bytes()) != "1" {
			t.Errorf("%s = raw %q, ok %v, err %v; want 1", lookup.name, raw.Bytes(), ok, err)
		}
	}
}

func BenchmarkNodePointerLookupOnly(b *testing.B) {
	src := benchmarkJSON()
	count, err := RequiredIndexEntries(src)
	if err != nil {
		b.Fatal(err)
	}
	index, err := BuildIndex(src, make([]IndexEntry, count))
	if err != nil {
		b.Fatal(err)
	}
	root := index.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		value, ok, err := root.Pointer("/items/2/message")
		if err != nil || !ok || value.Kind() != document.String {
			b.Fatal(value, ok, err)
		}
	}
}

func BenchmarkGetRawTrustedLookup(b *testing.B) {
	src := benchmarkJSON()
	pointer := MustCompilePointer("/items/2/message")
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		value, ok, err := pointer.GetRawTrusted(src)
		if err != nil || !ok || value.Kind() != document.String {
			b.Fatal(value, ok, err)
		}
	}
}
