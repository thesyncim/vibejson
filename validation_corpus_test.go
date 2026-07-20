package simdjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thesyncim/simdjson/document"
)

const jsonTestSuiteDir = "testdata/corpora/JSONTestSuite/test_parsing"

func addJSONTestSuiteSeeds(f *testing.F) {
	f.Helper()
	entries, err := os.ReadDir(jsonTestSuiteDir)
	if err != nil {
		f.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			f.Fatal(err)
		}
		if info.Size() > 4<<10 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(jsonTestSuiteDir, entry.Name()))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
}

func TestJSONTestSuite(t *testing.T) {
	entries, err := os.ReadDir(jsonTestSuiteDir)
	if err != nil {
		t.Fatal(err)
	}
	// The JSONTestSuite corpus is vendored under testdata and fixed at this
	// revision: 318 files total. Pinning the count catches an accidental
	// partial checkout or a corpus update that silently changes coverage, so
	// any change to the number below is a deliberate corpus refresh.
	if len(entries) != 318 {
		t.Fatalf("JSONTestSuite case count = %d, want 318", len(entries))
	}

	counts := map[byte]int{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		name := entry.Name()
		data, err := os.ReadFile(filepath.Join(jsonTestSuiteDir, name))
		if err != nil {
			t.Fatal(err)
		}
		want := jsonTestSuiteExpected(name)
		counts[name[0]]++
		t.Run(name, func(t *testing.T) {
			if oracle := strictJSONValid(data); oracle != want {
				t.Fatalf("strict stdlib oracle = %v, corpus policy = %v", oracle, want)
			}
			checkValidationConsistency(t, data, want)
		})
	}
	// The corpus splits by filename prefix: y_ must-accept (95), n_ must-reject
	// (188), i_ implementation-defined (35). These per-group counts are fixed by
	// the vendored corpus revision; pinning them proves every file was seen and
	// classified, so a miscounted or misnamed file fails here rather than
	// silently skipping coverage.
	if counts['y'] != 95 || counts['n'] != 188 || counts['i'] != 35 {
		t.Fatalf("JSONTestSuite groups = y:%d n:%d i:%d", counts['y'], counts['n'], counts['i'])
	}
}

func jsonTestSuiteExpected(name string) bool {
	switch name[0] {
	case 'y':
		return true
	case 'n':
		return false
	case 'i':
		return strings.HasPrefix(name, "i_number_") || name == "i_structure_500_nested_arrays.json"
	default:
		panic("unknown JSONTestSuite prefix: " + name)
	}
}

func checkValidationConsistency(t *testing.T, src []byte, want bool) {
	t.Helper()
	if got := Valid(src); got != want {
		t.Fatalf("Valid = %v, want %v (length %d)", got, want, len(src))
	}
	validateErr := Validate(src)
	if (validateErr == nil) != want {
		t.Fatalf("Validate error = %v, want valid %v", validateErr, want)
	}
	if validateErr != nil {
		assertSyntaxErrorPosition(t, validateErr, len(src))
	}

	entryCount, countErr := RequiredIndexEntries(src)
	if (countErr == nil) != want {
		t.Fatalf("RequiredIndexEntries = %d, %v, want valid %v", entryCount, countErr, want)
	}
	storageLen := len(src) + 1
	if want {
		storageLen = entryCount
	}
	storage := make([]IndexEntry, storageLen)
	index, indexErr := BuildIndex(src, storage)
	if (indexErr == nil) != want {
		t.Fatalf("BuildIndex error = %v, want valid %v", indexErr, want)
	}

	_, parseErr := Parse(src)
	if (parseErr == nil) != want {
		t.Fatalf("Parse error = %v, want valid %v", parseErr, want)
	}
	_, zeroCopyErr := parseOptionsZeroCopyForTest(src)
	if (zeroCopyErr == nil) != want {
		t.Fatalf("ParseOptions(ZeroCopy) error = %v, want valid %v", zeroCopyErr, want)
	}
	_, anyErr := decodeAnyForTest(src, DecoderOptions{ZeroCopy: true, UseNumber: true})
	if (anyErr == nil) != want {
		t.Fatalf("Decoder[any] error = %v, want valid %v", anyErr, want)
	}

	raw, rawOK, rawErr := GetRaw(src, "")
	if (rawErr == nil && rawOK) != want {
		t.Fatalf("GetRaw = ok:%v err:%v, want valid %v", rawOK, rawErr, want)
	}
	found, foundOK, findErr := ScanFirstRaw(src, "")
	if want && (findErr != nil || !foundOK) {
		t.Fatalf("ScanFirstRaw = ok:%v err:%v, want target", foundOK, findErr)
	}
	if findErr == nil && foundOK && !strictJSONValid(found.Bytes()) {
		t.Fatal("ScanFirstRaw returned a target that is not strict JSON")
	}

	compact, compactErr := AppendCompact(nil, src)
	if (compactErr == nil) != want {
		t.Fatalf("AppendCompact error = %v, want valid %v", compactErr, want)
	}
	if !want {
		return
	}

	trimmed := bytes.TrimSpace(src)
	if index.Len() != entryCount || !bytes.Equal(index.Root().Raw().Bytes(), trimmed) {
		t.Fatalf("index = entries:%d raw-length:%d, want entries:%d raw-length:%d", index.Len(), len(index.Root().Raw().Bytes()), entryCount, len(trimmed))
	}
	if !bytes.Equal(raw.Bytes(), trimmed) || !bytes.Equal(found.Bytes(), trimmed) {
		t.Fatal("root selectors changed the source value")
	}
	if !strictJSONValid(compact) {
		t.Fatal("AppendCompact produced invalid strict JSON")
	}
	verifyIndexStructure(t, index.Root())
}

func assertSyntaxErrorPosition(t *testing.T, err error, sourceLen int) {
	t.Helper()
	jsonErr, ok := err.(*SyntaxError)
	if !ok {
		t.Fatalf("syntax error type = %T, want *simdjson.SyntaxError", err)
	}
	if jsonErr.Offset < 0 || jsonErr.Offset > sourceLen || jsonErr.Line < 1 || jsonErr.Column < 1 || jsonErr.Message == "" {
		t.Fatalf("invalid syntax error position: %+v for source length %d", jsonErr, sourceLen)
	}
}

// strictJSONValid uses encoding/json for RFC 8259 grammar and independently
// rejects invalid UTF-8 and unpaired UTF-16 escapes. The latter are accepted by
// encoding/json v1 but intentionally rejected by simdjson and encoding/json v2.
func strictJSONValid(src []byte) bool {
	return json.Valid(src) && strictJSONStringEncoding(src)
}

func strictJSONStringEncoding(src []byte) bool {
	for i := 0; i < len(src); {
		if src[i] != '"' {
			i++
			continue
		}
		next, ok := strictJSONStringEnd(src, i)
		if !ok {
			return false
		}
		i = next
	}
	return true
}

func strictJSONStringEnd(src []byte, quote int) (int, bool) {
	for i := quote + 1; i < len(src); {
		switch c := src[i]; {
		case c == '"':
			return i + 1, true
		case c == '\\':
			i++
			if i >= len(src) {
				return i, false
			}
			switch src[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				i++
				continue
			case 'u':
			default:
				return i, false
			}
			u, ok := testHex4(src, i+1)
			if !ok {
				return i, false
			}
			i += 5
			switch {
			case 0xD800 <= u && u <= 0xDBFF:
				if i+6 > len(src) || src[i] != '\\' || src[i+1] != 'u' {
					return i, false
				}
				lo, ok := testHex4(src, i+2)
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return i, false
				}
				i += 6
			case 0xDC00 <= u && u <= 0xDFFF:
				return i, false
			}
		case c < utf8.RuneSelf:
			if c < 0x20 {
				return i, false
			}
			i++
		default:
			r, size := utf8.DecodeRune(src[i:])
			if r == utf8.RuneError && size == 1 {
				return i, false
			}
			i += size
		}
	}
	return len(src), false
}

func testHex4(src []byte, start int) (uint16, bool) {
	if start+4 > len(src) {
		return 0, false
	}
	var value uint16
	for _, c := range src[start : start+4] {
		value <<= 4
		switch {
		case '0' <= c && c <= '9':
			value |= uint16(c - '0')
		case 'a' <= c && c <= 'f':
			value |= uint16(c-'a') + 10
		case 'A' <= c && c <= 'F':
			value |= uint16(c-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func TestSIMDJSONUnicodeCases(t *testing.T) {
	// Provenance: CPP-UTF8-TEST-001.
	// Ported from simdjson tests/unicode_tests.cpp at commit
	// 9b33047a878264250c5361f865d0b2da86217d14. That source credits the
	// Autobahn WebSocket TestSuite for its additional numbered cases; the
	// exact Autobahn revision was not recorded upstream.
	valid := [][]byte{
		[]byte("a"),
		{0xC3, 0xB1},
		{0xE2, 0x82, 0xA1},
		{0xF0, 0x90, 0x8C, 0xBC},
		{0xC2, 0x80},
		{0xF0, 0x90, 0x80, 0x80},
		{0xEE, 0x80, 0x80},
		{0xEF, 0xBB, 0xBF},
	}
	invalid := [][]byte{
		{0xC3, 0x28},
		{0xA0, 0xA1},
		{0xE2, 0x28, 0xA1},
		{0xE2, 0x82, 0x28},
		{0xF0, 0x28, 0x8C, 0xBC},
		{0xF0, 0x90, 0x28, 0xBC},
		{0xF0, 0x28, 0x8C, 0x28},
		{0xC0, 0x9F},
		{0xF5, 0xFF, 0xFF, 0xFF},
		{0xED, 0xA0, 0x81},
		{0xF8, 0x90, 0x80, 0x80, 0x80},
		{0xCE},
		{0xCE, 0xBA, 0xE1},
		{0xCE, 0xBA, 0xE1, 0xBD},
		{0xDF},
		{0xEF, 0xBF},
		{0x80},
	}
	boundaries := []int{0, 1, 7, 8, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 128, 129}
	for _, prefix := range boundaries {
		for index, sequence := range valid {
			src := quotedAtBoundary(prefix, sequence)
			if !ValidString(src) || !strictJSONValid(src) {
				t.Fatalf("valid UTF-8 sequence %d rejected at prefix %d: %x", index, prefix, sequence)
			}
		}
		for index, sequence := range invalid {
			src := quotedAtBoundary(prefix, sequence)
			if ValidString(src) || strictJSONValid(src) {
				t.Fatalf("invalid UTF-8 sequence %d accepted at prefix %d: %x", index, prefix, sequence)
			}
		}
	}
}

func quotedAtBoundary(prefix int, sequence []byte) []byte {
	src := make([]byte, 1, prefix+len(sequence)+2)
	src[0] = '"'
	src = append(src, strings.Repeat("a", prefix)...)
	src = append(src, sequence...)
	return append(src, '"')
}

func TestValidationDepthBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		depth int
		want  bool
	}{
		{"below", defaultMaxDepth - 1, true},
		{"at", defaultMaxDepth, true},
		{"above", defaultMaxDepth + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(strings.Repeat("[", tc.depth) + "0" + strings.Repeat("]", tc.depth))
			if got := Valid(src); got != tc.want {
				t.Fatalf("Valid at depth %d = %v, want %v", tc.depth, got, tc.want)
			}
			if oracle := strictJSONValid(src); oracle != tc.want {
				t.Fatalf("strict oracle at depth %d = %v, want %v", tc.depth, oracle, tc.want)
			}
		})
	}
}

func checkScalarValidatorAgreement(t *testing.T, src []byte) {
	t.Helper()
	wantJSON := strictJSONValid(src)
	wantNumber := wantJSON && len(src) != 0 && (src[0] == '-' || ('0' <= src[0] && src[0] <= '9')) && !hasJSONSpaceEdges(src)
	if got := ValidNumber(src); got != wantNumber {
		t.Fatalf("ValidNumber = %v, strict oracle = %v", got, wantNumber)
	}
	numberErr := ValidateNumber(src)
	if (numberErr == nil) != wantNumber {
		t.Fatalf("ValidateNumber error = %v, want valid %v", numberErr, wantNumber)
	}
	if numberErr != nil {
		assertSyntaxErrorPosition(t, numberErr, len(src))
	}

	wantString := wantJSON && len(src) >= 2 && src[0] == '"' && src[len(src)-1] == '"' && !hasJSONSpaceEdges(src)
	if got := ValidString(src); got != wantString {
		t.Fatalf("ValidString = %v, strict oracle = %v", got, wantString)
	}
	stringErr := ValidateString(src)
	if (stringErr == nil) != wantString {
		t.Fatalf("ValidateString error = %v, want valid %v", stringErr, wantString)
	}
	if stringErr != nil {
		assertSyntaxErrorPosition(t, stringErr, len(src))
	}
}

func hasJSONSpaceEdges(src []byte) bool {
	return len(src) != 0 && (isJSONSpace(src[0]) || isJSONSpace(src[len(src)-1]))
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func FuzzIndexNavigation(f *testing.F) {
	for _, seed := range []struct {
		src     []byte
		pointer string
	}{
		{[]byte(`null`), ""},
		{[]byte(`{"a/b":{"~key":[0,1]}}`), "/a~1b/~0key/1"},
		{[]byte(`{"dup":1,"dup":2}`), "/dup"},
		{[]byte(`[0,{"x":"y"}]`), "/1/x"},
		{[]byte(`[0]`), "/01"},
		{[]byte(`{}`), "/~2"},
	} {
		f.Add(byte(0), seed.src, seed.pointer, uint16(0))
	}
	for _, seed := range []struct {
		src []byte
		cap uint16
	}{
		{[]byte(`null`), 0},
		{[]byte(`null`), 1},
		{[]byte(`[0,1]`), 2},
		{[]byte(`[0,1]`), 3},
		{[]byte(`{"a":[true,null]}`), 7},
	} {
		f.Add(byte(1), seed.src, "", seed.cap)
	}
	f.Fuzz(func(t *testing.T, mode byte, src []byte, pointer string, requestedCap uint16) {
		switch mode & 1 {
		case 0:
			if len(src) > 1<<15 || len(pointer) > 1<<12 || !strictJSONValid(src) {
				t.Skip()
			}
			checkPointerConsistency(t, src, pointer)
		case 1:
			if len(src) > 1<<12 || !strictJSONValid(src) {
				t.Skip()
			}
			checkIndexStorageBoundary(t, src, requestedCap)
		}
	})
}

func checkPointerConsistency(t *testing.T, src []byte, pointer string) {
	t.Helper()
	compiled, compileErr := CompilePointer(pointer)
	dynamicRaw, dynamicOK, dynamicErr := GetRaw(src, pointer)
	if compileErr != nil {
		if dynamicErr == nil {
			t.Fatal("GetRaw accepted a pointer rejected by CompilePointer")
		}
		return
	}
	compiledRaw, compiledOK, compiledErr := compiled.GetRaw(src)
	assertRawLookupEqual(t, "GetRaw", dynamicRaw, dynamicOK, dynamicErr, compiledRaw, compiledOK, compiledErr)

	dynamicFound, dynamicFoundOK, dynamicFindErr := ScanFirstRaw(src, pointer)
	compiledFound, compiledFoundOK, compiledFindErr := compiled.ScanFirstRaw(src)
	assertRawLookupEqual(t, "ScanFirstRaw", dynamicFound, dynamicFoundOK, dynamicFindErr, compiledFound, compiledFoundOK, compiledFindErr)

	count, err := RequiredIndexEntries(src)
	if err != nil {
		t.Fatal(err)
	}
	storage := make([]IndexEntry, count)
	tape, err := BuildIndex(src, storage)
	if err != nil {
		t.Fatal(err)
	}
	tapeDynamic, tapeDynamicOK, tapeDynamicErr := tape.Pointer(pointer)
	tapeCompiled, tapeCompiledOK, tapeCompiledErr := tape.PointerCompiled(compiled)
	assertIndexLookupEqual(t, tapeDynamic, tapeDynamicOK, tapeDynamicErr, tapeCompiled, tapeCompiledOK, tapeCompiledErr)
	if (dynamicErr == nil) != (tapeDynamicErr == nil) || dynamicOK != tapeDynamicOK {
		t.Fatalf("raw/tape lookup status differs: raw=(%v,%v) tape=(%v,%v)", dynamicOK, dynamicErr, tapeDynamicOK, tapeDynamicErr)
	}
	if dynamicOK && !bytes.Equal(dynamicRaw.Bytes(), tapeDynamic.Raw().Bytes()) {
		t.Fatal("raw and tape pointer targets differ")
	}
}

func checkIndexStorageBoundary(t *testing.T, src []byte, requestedCap uint16) {
	t.Helper()
	count, err := RequiredIndexEntries(src)
	if err != nil {
		t.Fatal(err)
	}
	capacity := int(requestedCap) % (count + 2)
	storage := make([]IndexEntry, capacity)
	index, err := BuildIndex(src, storage)
	if capacity < count {
		if !errors.Is(err, document.ErrIndexFull) {
			t.Fatalf("capacity %d/%d error = %v, want document.ErrIndexFull", capacity, count, err)
		}
		return
	}
	if err != nil || index.Len() != count {
		t.Fatalf("capacity %d/%d result = len:%d err:%v", capacity, count, index.Len(), err)
	}
}

func assertRawLookupEqual(t *testing.T, name string, a RawValue, aOK bool, aErr error, b RawValue, bOK bool, bErr error) {
	t.Helper()
	if (aErr == nil) != (bErr == nil) || aOK != bOK || !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("%s dynamic=(%q,%v,%v) compiled=(%q,%v,%v)", name, a.Bytes(), aOK, aErr, b.Bytes(), bOK, bErr)
	}
}

func assertIndexLookupEqual(t *testing.T, a Node, aOK bool, aErr error, b Node, bOK bool, bErr error) {
	t.Helper()
	if (aErr == nil) != (bErr == nil) || aOK != bOK || !bytes.Equal(a.Raw().Bytes(), b.Raw().Bytes()) {
		t.Fatalf("tape dynamic=(%q,%v,%v) compiled=(%q,%v,%v)", a.Raw().Bytes(), aOK, aErr, b.Raw().Bytes(), bOK, bErr)
	}
}

func checkTransforms(t *testing.T, src []byte) {
	t.Helper()
	if len(src) > 1<<15 || !strictJSONValid(src) {
		return
	}
	compact, err := AppendCompact(make([]byte, 0, len(src)), src)
	if err != nil || !strictJSONValid(compact) {
		t.Fatalf("AppendCompact = %q, %v", compact, err)
	}
	compactAgain, err := AppendCompact(make([]byte, 0, len(compact)), compact)
	if err != nil || !bytes.Equal(compactAgain, compact) {
		t.Fatalf("compact is not idempotent: %q -> %q, %v", compact, compactAgain, err)
	}
	pretty, err := Indent(src, "", "  ")
	if err != nil || !strictJSONValid(pretty) {
		t.Fatalf("Indent produced invalid JSON: %q, %v", pretty, err)
	}
	prettyCompact, err := AppendCompact(make([]byte, 0, len(compact)), pretty)
	if err != nil {
		t.Fatalf("compacting indented JSON: %v", err)
	}
	wantCanonical, err := Canonicalize(compact)
	if err != nil {
		t.Fatalf("canonicalizing compact JSON: %v", err)
	}
	gotCanonical, err := Canonicalize(prettyCompact)
	if err != nil || !bytes.Equal(gotCanonical, wantCanonical) {
		t.Fatalf("Indent changed semantic form: %q -> %q, %v", wantCanonical, gotCanonical, err)
	}
	canonicalAgain, err := Canonicalize(gotCanonical)
	if err != nil || !bytes.Equal(canonicalAgain, gotCanonical) {
		t.Fatalf("canonical form is not idempotent: %q -> %q, %v", gotCanonical, canonicalAgain, err)
	}
}
