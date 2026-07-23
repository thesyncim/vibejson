package slopjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/thesyncim/slopjson/document"
)

// trustedStaticPointers is the fixed lookup battery applied to every
// differential document: the empty pointer, present and absent members,
// array indices, deep paths, tilde escapes, and invalid pointer syntax.
var trustedStaticPointers = []string{
	"",
	"/",
	"/a",
	"/a/b",
	"/a/b/c",
	"/b",
	"/x/y",
	"/missing",
	"/0",
	"/1",
	"/2/z",
	"/50",
	"/dup",
	"/other/dup",
	"/outer/inner/deep",
	"/outer/inner",
	"/outer/list/1/z",
	"/a~1b",
	"/a~0b",
	"/héllo",
	"/😀",
	"/k\n",
	"/-",
	"/00",
	"/abc",
	"/member0004",
	"/x/y/0",
	"no-leading-slash",
	"/bad~2escape",
	"/dangling~",
}

// trustedDerivedPointers walks doc's decoded tree and returns a pointer for
// every value up to limit, so present-path coverage follows each document's
// actual shape.
func trustedDerivedPointers(tb testing.TB, doc []byte, limit int) []string {
	tb.Helper()
	var root any
	if err := json.Unmarshal(doc, &root); err != nil {
		tb.Fatalf("corpus document is not valid JSON: %v", err)
	}
	escaper := strings.NewReplacer("~", "~0", "/", "~1")
	pointers := []string{""}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if len(pointers) >= limit {
			return
		}
		switch v := v.(type) {
		case map[string]any:
			for key, child := range v {
				p := prefix + "/" + escaper.Replace(key)
				pointers = append(pointers, p)
				walk(p, child)
			}
		case []any:
			for i, child := range v {
				p := prefix + "/" + strconv.Itoa(i)
				pointers = append(pointers, p)
				walk(p, child)
			}
		}
	}
	walk("", root)
	if len(pointers) > limit {
		pointers = pointers[:limit]
	}
	return pointers
}

// assertTrustedMatchesScanFirst verifies the trusted contract's parity half:
// given valid src, ScanFirstRawTrusted and its compiled form return the same
// presence, bytes, and error acceptance as ScanFirstRaw.
func assertTrustedMatchesScanFirst(t *testing.T, src []byte, pointer string) {
	t.Helper()
	want, wantOK, wantErr := ScanFirstRaw(src, pointer)
	got, gotOK, gotErr := ScanFirstRawTrusted(src, pointer)
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("pointer %q: error mismatch: validating=%v trusted=%v\nsrc=%.120q", pointer, wantErr, gotErr, src)
	}
	if wantOK != gotOK {
		t.Fatalf("pointer %q: ok mismatch: validating=%v trusted=%v\nsrc=%.120q", pointer, wantOK, gotOK, src)
	}
	if !bytes.Equal(want.Bytes(), got.Bytes()) {
		t.Fatalf("pointer %q: bytes mismatch:\nvalidating=%q\ntrusted=%q\nsrc=%.120q", pointer, want.Bytes(), got.Bytes(), src)
	}
	compiled, err := CompilePointer(pointer)
	if err != nil {
		if gotErr == nil {
			t.Fatalf("pointer %q: CompilePointer rejects but trusted accepted", pointer)
		}
		return
	}
	cgot, cok, cerr := compiled.ScanFirstRawTrusted(src)
	if cok != gotOK || (cerr == nil) != (gotErr == nil) || !bytes.Equal(cgot.Bytes(), got.Bytes()) {
		t.Fatalf("pointer %q: compiled trusted disagrees with package form", pointer)
	}
	last, lastOK, lastErr := compiled.GetRaw(src)
	trustedLast, trustedLastOK, trustedLastErr := compiled.getRawTrusted(src)
	if trustedLastOK != lastOK || (trustedLastErr == nil) != (lastErr == nil) ||
		!bytes.Equal(trustedLast.Bytes(), last.Bytes()) {
		t.Fatalf("pointer %q: trusted last-wins disagrees with GetRaw", pointer)
	}
}

// TestScanFirstRawTrustedMatchesValidatingOnCorpus pins the parity half of
// the trusted contract: given every valid corpus document, when any battery
// pointer is resolved, then ScanFirstRawTrusted returns byte-identical
// results to ScanFirstRaw.
func TestScanFirstRawTrustedMatchesValidatingOnCorpus(t *testing.T) {
	docs := append([]string{}, keyHashCorpus...)
	docs = append(docs,
		keyHashWideDoc(64, "pad"),
		keyHashWideDoc(300, ""),
		`  [ 1 , { "a" : [ true , null , { "b" : "cAd" } ] } , "tail" ] `,
		`{"esc":"a\\\"b","empty":{},"blank":[],"num":-1.25e+9,"t":true,"n":null}`,
		"{\n\t\"a\": {\n\t\t\"b\": [1, 2, 3],\n\t\t\"c\": \"multi\\nline\"\n\t}\n}",
	)
	for _, doc := range docs {
		src := []byte(doc)
		pointers := append([]string{}, trustedStaticPointers...)
		pointers = append(pointers, trustedDerivedPointers(t, src, 200)...)
		for _, pointer := range pointers {
			assertTrustedMatchesScanFirst(t, src, pointer)
		}
	}
}

// TestScanFirstRawTrustedMatchesValidatingOnJSONTestSuite runs the same
// parity assertion across every accepted JSONTestSuite document.
func TestScanFirstRawTrustedMatchesValidatingOnJSONTestSuite(t *testing.T) {
	entries, err := os.ReadDir(jsonTestSuiteDir)
	if err != nil {
		t.Skip("JSONTestSuite corpus not present")
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" || !strings.HasPrefix(name, "y_") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(jsonTestSuiteDir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, pointer := range trustedStaticPointers {
			assertTrustedMatchesScanFirst(t, src, pointer)
		}
	}
}

// TestScanFirstRawTrustedMatchesValidatingOnCanonicalCorpus extends the
// parity battery to the optional canonical corpus files when they are
// present, twitter.json among them.
func TestScanFirstRawTrustedMatchesValidatingOnCanonicalCorpus(t *testing.T) {
	for _, name := range []string{"twitter.json", "canada.json", "citm_catalog.json"} {
		t.Run(name, func(t *testing.T) {
			src := loadSimdjsonCorpus(t, name)
			pointers := append([]string{}, trustedStaticPointers...)
			pointers = append(pointers, trustedDerivedPointers(t, src, 400)...)
			for _, pointer := range pointers {
				assertTrustedMatchesScanFirst(t, src, pointer)
			}
		})
	}
}

// TestScanFirstRawTrustedDuplicateKeyContract pins the duplicate-key rules
// of both raw contracts on one fixture set: trusted scans share
// ScanFirstRaw's first-resolving-member rule, while GetRaw keeps
// encoding/json's last-occurrence rule.
func TestScanFirstRawTrustedDuplicateKeyContract(t *testing.T) {
	cases := []struct {
		src, pointer    string
		first, last     string
		firstOK, lastOK bool
	}{
		{`{"a":1,"a":2}`, "/a", "1", "2", true, true},
		{`{"a":{"b":1},"a":{"b":2}}`, "/a/b", "1", "2", true, true},
		// The first "a" does not resolve /a/b; the first-match rule commits
		// to the first member under which the whole pointer resolves.
		{`{"a":1,"a":{"b":2},"a":{"b":3}}`, "/a/b", "2", "3", true, true},
		{`{"a":{"b":1},"a":2}`, "/a/b", "1", "", true, false},
		{`{"dup":1,"other":{"dup":2},"dup":3}`, "/other/dup", "2", "2", true, true},
		{`{"a":[{"b":1},{"b":2}]}`, "/a/1/b", "2", "2", true, true},
		{`{"a/b":1,"a\u002fb":2}`, "/a~1b", "1", "2", true, true},
	}
	for _, tc := range cases {
		src := []byte(tc.src)
		got, ok, err := ScanFirstRawTrusted(src, tc.pointer)
		if err != nil || ok != tc.firstOK || got.String() != tc.first {
			t.Errorf("ScanFirstRawTrusted(%q, %q) = %q, %v, %v; want %q, %v", tc.src, tc.pointer, got.String(), ok, err, tc.first, tc.firstOK)
		}
		assertTrustedMatchesScanFirst(t, src, tc.pointer)
		last, ok, err := GetRaw(src, tc.pointer)
		if err != nil || ok != tc.lastOK || last.String() != tc.last {
			t.Errorf("GetRaw(%q, %q) = %q, %v, %v; want %q, %v", tc.src, tc.pointer, last.String(), ok, err, tc.last, tc.lastOK)
		}
		compiled, err := CompilePointer(tc.pointer)
		if err != nil {
			t.Fatal(err)
		}
		trustedLast, trustedOK, trustedErr := compiled.getRawTrusted(src)
		if trustedErr != nil || trustedOK != tc.lastOK || trustedLast.String() != tc.last {
			t.Errorf("getRawTrusted(%q, %q) = %q, %v, %v; want %q, %v", tc.src, tc.pointer, trustedLast.String(), trustedOK, trustedErr, tc.last, tc.lastOK)
		}
	}
}

// TestScanFirstRawTrustedStopsAtTarget pins the early-exit half of the
// contract that parity tests cannot see: bytes after the resolved target are
// never inspected, so garbage there does not affect the result. ScanFirstRaw
// shares this property and must agree.
func TestScanFirstRawTrustedStopsAtTarget(t *testing.T) {
	cases := []struct {
		src, pointer, want string
	}{
		{`{"a":1} utter garbage \x00`, "/a", "1"},
		{`{"a":{"b":[1,2]},"broken":"unclosed`, "/a/b/1", "2"},
		{`[10,20,30,{{{{`, "/1", "20"},
	}
	for _, tc := range cases {
		src := []byte(tc.src)
		for name, lookup := range map[string]func([]byte, string) (RawValue, bool, error){
			"trusted":    ScanFirstRawTrusted,
			"validating": ScanFirstRaw,
		} {
			got, ok, err := lookup(src, tc.pointer)
			if err != nil || !ok || got.String() != tc.want {
				t.Errorf("%s(%q, %q) = %q, %v, %v; want %q", name, tc.src, tc.pointer, got.String(), ok, err, tc.want)
			}
		}
	}
}

// assertTrustedBounded verifies the safety half of the contract on one
// input: the call returns, and any reported span is non-empty and lies
// inside src.
func assertTrustedBounded(t *testing.T, src []byte, pointer string) {
	t.Helper()
	raw, ok, _ := ScanFirstRawTrusted(src, pointer)
	if !ok {
		return
	}
	b := raw.Bytes()
	if len(b) == 0 {
		t.Fatalf("pointer %q: ok with empty span\nsrc=%.120q", pointer, src)
	}
	s0 := uintptr(unsafe.Pointer(unsafe.SliceData(src)))
	b0 := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	if b0 < s0 || b0+uintptr(len(b)) > s0+uintptr(len(src)) {
		t.Fatalf("pointer %q: span escapes src bounds\nsrc=%.120q", pointer, src)
	}
}

// TestScanFirstRawTrustedAdversarial feeds the trusted scan the malformed
// shapes its safety contract names — truncations at every byte, unclosed
// strings with trailing escapes, NUL bytes, invalid UTF-8, stray structure —
// and requires bounded, terminating behavior on all of them.
func TestScanFirstRawTrustedAdversarial(t *testing.T) {
	nested := []byte(`{"a":[1,{"k":"v\nA\\"},true,null,-1.5e+3],"b":{"c":["x","y\\\"z"]},"d":"tail"}`)
	fixtures := [][]byte{
		nil,
		[]byte(" "),
		[]byte(`"unclosed`),
		[]byte(`"trailing\`),
		[]byte(`"trailing\\\`),
		[]byte(`{"a`),
		[]byte(`{"a"`),
		[]byte(`{"a":`),
		[]byte(`{"a":}`),
		[]byte(`{"a":1,]`),
		[]byte(`[}`),
		[]byte(`[,,,]`),
		[]byte(`{::}`),
		[]byte("{\"a\":\"\x00\x01\x02\"}"),
		[]byte("\x00\x00\x00"),
		[]byte("{\"\xff\xfe\":1}"),
		[]byte("[\"\xc3\x28\", \"\xf0\x9f\"]"),
		[]byte(`{"a":01,"b":+1,"c":truex,"d":nul}`),
		bytes.Repeat([]byte("["), 64),
		bytes.Repeat([]byte("{\"k\":"), 64),
	}
	for i := 0; i <= len(nested); i++ {
		fixtures = append(fixtures, nested[:i])
	}
	pointers := []string{"", "/a", "/a/1/k", "/b/c/1", "/d", "/0", "/64", "/missing"}
	for _, src := range fixtures {
		for _, pointer := range pointers {
			assertTrustedBounded(t, src, pointer)
		}
	}
}

// TestScanFirstRawTrustedDepthLimit pins that trusted scans keep the
// validating depth limit: a valid document nested past MaxDepth is rejected
// by both spellings, whether the excess depth sits on the navigated path or
// inside a skipped subtree, and documents at the limit pass both.
func TestScanFirstRawTrustedDepthLimit(t *testing.T) {
	deepDoc := func(depth int) []byte {
		return append(append(bytes.Repeat([]byte("["), depth), '1'), bytes.Repeat([]byte("]"), depth)...)
	}
	for _, tc := range []struct {
		name    string
		src     []byte
		pointer string
		opts    Options
		wantErr bool
	}{
		{"at-limit", deepDoc(6), "", Options{MaxDepth: 6}, false},
		{"past-limit", deepDoc(7), "", Options{MaxDepth: 6}, true},
		{"past-limit-on-path", deepDoc(7), "/0/0", Options{MaxDepth: 6}, true},
		{"past-limit-in-skipped-sibling", []byte(`{"deep":[[[[1]]]],"x":2}`), "/x", Options{MaxDepth: 3}, true},
		{"default-at-limit", deepDoc(defaultMaxDepth), "", Options{}, false},
		{"default-past-limit", deepDoc(defaultMaxDepth + 1), "", Options{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, gotErr := ScanFirstRawTrustedOptions(tc.src, tc.pointer, tc.opts)
			_, _, wantErr := ScanFirstRawOptions(tc.src, tc.pointer, tc.opts)
			if (gotErr != nil) != tc.wantErr || (wantErr != nil) != tc.wantErr {
				t.Fatalf("depth errors: trusted=%v validating=%v, want error=%v", gotErr, wantErr, tc.wantErr)
			}
			if tc.wantErr {
				var syn *SyntaxError
				if !errors.As(gotErr, &syn) {
					t.Fatalf("trusted depth error is %T, want *SyntaxError", gotErr)
				}
			}
		})
	}
}

// TestScanFirstRawTrustedPointerErrors pins that pointer diagnostics survive
// trusting the document: syntax errors and array-index errors return
// document.PointerError exactly as the validating spelling does.
func TestScanFirstRawTrustedPointerErrors(t *testing.T) {
	src := []byte(`{"a":[1,2,3]}`)
	for _, pointer := range []string{"x", "/~2", "/~", "/a/x", "/a/01", "/a/-"} {
		_, gotOK, gotErr := ScanFirstRawTrusted(src, pointer)
		_, wantOK, wantErr := ScanFirstRaw(src, pointer)
		if gotOK != wantOK || (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("pointer %q: trusted=(%v, %v) validating=(%v, %v)", pointer, gotOK, gotErr, wantOK, wantErr)
		}
		if wantErr == nil {
			continue
		}
		var perr *document.PointerError
		if !errors.As(gotErr, &perr) {
			t.Fatalf("pointer %q: trusted error is %T, want *document.PointerError", pointer, gotErr)
		}
	}
}

// FuzzScanFirstRawTrusted owns the arbitrary-byte safety contract of trusted
// pointer scans: any input bytes and pointer must return without panicking,
// any reported span must lie inside src, and on documents the validator
// accepts the result must match ScanFirstRaw exactly.
func FuzzScanFirstRawTrusted(f *testing.F) {
	seeds := []struct {
		src, pointer string
	}{
		{`{"a":[1,{"k":"v"},true],"b":{"c":"d"}}`, "/a/1/k"},
		{`{"a":1,"a":{"b":2}}`, "/a/b"},
		{`{"k\n":1,"k
":2}`, "/k\n"},
		{`{"a/b":1,"a~b":2}`, "/a~1b"},
		{`[[[[[1]]]]]`, "/0/0/0/0/0"},
		{`{"a":"unclosed`, "/a"},
		{`{"a":"esc\`, "/a"},
		{"{\"\xff\":1}", ""},
		{`[0,1,2,3,4,5,6,7,8,9]`, "/9"},
		{`{"a":1} trailing garbage`, "/a"},
		{"", ""},
		{`{`, "/a"},
	}
	for _, seed := range seeds {
		f.Add([]byte(seed.src), seed.pointer)
	}
	f.Fuzz(func(t *testing.T, src []byte, pointer string) {
		if len(src) > 1<<16 || len(pointer) > 1<<10 {
			t.Skip()
		}
		raw, ok, err := ScanFirstRawTrusted(src, pointer)
		if ok {
			b := raw.Bytes()
			if len(b) == 0 {
				t.Fatalf("ok with empty span: src=%.120q pointer=%q", src, pointer)
			}
			s0 := uintptr(unsafe.Pointer(unsafe.SliceData(src)))
			b0 := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
			if b0 < s0 || b0+uintptr(len(b)) > s0+uintptr(len(src)) {
				t.Fatalf("span escapes src bounds: src=%.120q pointer=%q", src, pointer)
			}
		}
		want, wantOK, wantErr := ScanFirstRaw(src, pointer)
		if (wantErr == nil) != (err == nil) && Valid(src) {
			t.Fatalf("error acceptance differs on valid src: validating=%v trusted=%v\nsrc=%.120q pointer=%q", wantErr, err, src, pointer)
		}
		if !Valid(src) {
			return
		}
		if wantOK != ok || !bytes.Equal(want.Bytes(), raw.Bytes()) {
			t.Fatalf("valid-src divergence: validating=(%q, %v) trusted=(%q, %v)\nsrc=%.120q pointer=%q", want.Bytes(), wantOK, raw.Bytes(), ok, src, pointer)
		}
	})
}
