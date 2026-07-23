package slopjson

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/thesyncim/slopjson/document"
)

// flatEquivalenceDocs are adversarial documents for proving that the
// auto-selected flat stride never changes iteration or lookup semantics:
// nested and empty containers, single elements, escaped and duplicate keys,
// flat containers holding empty containers, and mixed shapes.
var flatEquivalenceDocs = []string{
	`[]`,
	`{}`,
	`[[]]`,
	`[{}]`,
	`[[],{},[]]`,
	`[42]`,
	`[1,2,3]`,
	`[1,[2],3]`,
	`[1,"two",true,null,3.5]`,
	`[[1,2],[3],[]]`,
	`[{"a":1},{"a":2}]`,
	`[1,{"a":[2,3]},4]`,
	`[[[[1]]]]`,
	`{"k":null}`,
	`{"a":1}`,
	`{"a":1,"b":2,"c":3}`,
	`{"a":{"b":2},"c":3}`,
	`{"a":[],"b":{}}`,
	`{"a":1,"a":2}`,
	`{"a":1,"b":[1,2],"a":3}`,
	`{"ab":1,"ab":2}`,
	`{"\"":1,"\\":2}`,
	`{"outer":{"a":1,"a":2,"b":[{"x":[]},7]},"outer":[0]}`,
	`["a\nb","c"]`,
	"\t[ 1 ,\n 2 , {\r\"a\" : [ ] } ]\n",
	`[` + strings.Repeat(`5,`, 99) + `5]`,
	`{` + strings.Repeat(`"k":0,`, 99) + `"k":1}`,
}

// refArrayChildren chases each element's recorded span through the raw
// entries, the layout contract the iterators must reproduce.
func refArrayChildren(entries []IndexEntry, container int) []int {
	count := int(entries[container].Count())
	children := make([]int, 0, count)
	child := container + 1
	for range count {
		children = append(children, child)
		child += int(entries[child].next)
	}
	return children
}

// refObjectMembers chases each value's recorded span through the raw entries.
func refObjectMembers(entries []IndexEntry, container int) (keys, values []int) {
	count := int(entries[container].Count())
	key := container + 1
	for range count {
		keys = append(keys, key)
		values = append(values, key+1)
		key += 1 + int(entries[key+1].next)
	}
	return keys, values
}

func nodeAtEntry(t *testing.T, tape Index, index int) Node {
	t.Helper()
	return Node{src: unsafe.SliceData(tape.src), entry: &tape.entries[index]}
}

func checkArrayAgainstReference(t *testing.T, tape Index, container int) {
	t.Helper()
	entries := tape.entries
	node := nodeAtEntry(t, tape, container)
	want := refArrayChildren(entries, container)

	iter, ok := node.ArrayIter()
	if !ok {
		t.Fatalf("ArrayIter failed on array entry %d", container)
	}
	for i, wantIndex := range want {
		got, ok := iter.Next()
		if !ok {
			t.Fatalf("ArrayIter.Next ended at element %d, want %d elements", i, len(want))
		}
		if got.entry != &entries[wantIndex] {
			t.Fatalf("ArrayIter element %d = entry %p, want entry %d", i, got.entry, wantIndex)
		}
		byIndex, ok := node.Index(i)
		if !ok || byIndex.entry != &entries[wantIndex] {
			t.Fatalf("Index(%d) = (%p, %v), want entry %d", i, byIndex.entry, ok, wantIndex)
		}
	}
	if _, ok := iter.Next(); ok {
		t.Fatalf("ArrayIter yielded more than %d elements", len(want))
	}
	if _, ok := node.Index(len(want)); ok {
		t.Fatalf("Index(%d) succeeded past the end", len(want))
	}
}

func checkObjectAgainstReference(t *testing.T, tape Index, container int) {
	t.Helper()
	entries := tape.entries
	node := nodeAtEntry(t, tape, container)
	wantKeys, wantValues := refObjectMembers(entries, container)

	iter, ok := node.ObjectIter()
	if !ok {
		t.Fatalf("ObjectIter failed on object entry %d", container)
	}
	for i := range wantKeys {
		key, value, ok := iter.Next()
		if !ok {
			t.Fatalf("ObjectIter.Next ended at member %d, want %d members", i, len(wantKeys))
		}
		if key.entry != &entries[wantKeys[i]] || value.entry != &entries[wantValues[i]] {
			t.Fatalf("ObjectIter member %d = (%p, %p), want entries (%d, %d)",
				i, key.entry, value.entry, wantKeys[i], wantValues[i])
		}
	}
	if _, _, ok := iter.Next(); ok {
		t.Fatalf("ObjectIter yielded more than %d members", len(wantKeys))
	}

	// Get must return the last member whose decoded key matches, whether or
	// not the flat scan is selected.
	lastByKey := map[string]int{}
	for i, keyIndex := range wantKeys {
		decoded := nodeKeyString(nodeAtEntry(t, tape, keyIndex))
		lastByKey[decoded] = wantValues[i]
	}
	for decoded, wantValue := range lastByKey {
		got, ok := node.Get(decoded)
		if !ok || got.entry != &entries[wantValue] {
			t.Fatalf("Get(%q) = (%p, %v), want entry %d", decoded, got.entry, ok, wantValue)
		}
	}
	if _, ok := node.Get("\x00 definitely absent"); ok {
		t.Fatalf("Get on absent key succeeded")
	}
}

// TestIteratorFlatAutoSelectEquivalence proves the auto-selected flat stride
// yields exactly the elements a span chase yields, in order, for every
// container of every adversarial document, alongside Index and Get.
func TestIteratorFlatAutoSelectEquivalence(t *testing.T) {
	for _, doc := range flatEquivalenceDocs {
		src := []byte(doc)
		storage := make([]IndexEntry, len(src)+1)
		tape, err := BuildIndex(src, storage)
		if err != nil {
			t.Fatalf("BuildIndex(%q): %v", doc, err)
		}
		for i := range tape.entries {
			switch tape.entries[i].Kind() {
			case document.Array:
				checkArrayAgainstReference(t, tape, i)
			case document.Object:
				checkObjectAgainstReference(t, tape, i)
			}
		}
	}
}

// TestTapeBuildersAgree proves the fast and the diagnostic tape builders
// produce identical entries — spans, counts, kinds, and flags including the
// plain-integer tag — for every adversarial document.
func TestTapeBuildersAgree(t *testing.T) {
	docs := append([]string{}, flatEquivalenceDocs...)
	docs = append(docs,
		`[0,-0,1,1.5,-1.5,1e2,-1E-2,0.0,12345678901234567890,3.14]`,
		`{"n":-9223372036854775808,"f":2.5e-3}`,
	)
	for _, doc := range docs {
		src := []byte(doc)
		fast := tapeBuilder{
			src:      src,
			base:     unsafe.Pointer(unsafe.SliceData(src)),
			entries:  make([]IndexEntry, 0, len(src)+1),
			parent:   noTapeParent,
			maxDepth: defaultMaxDepth,
		}
		if status := fast.parseFast(); status != tapeParseOK {
			t.Fatalf("parseFast(%q) = %v", doc, status)
		}
		slow := tapeBuilder{
			src:      src,
			base:     unsafe.Pointer(unsafe.SliceData(src)),
			entries:  make([]IndexEntry, 0, len(src)+1),
			parent:   noTapeParent,
			maxDepth: defaultMaxDepth,
		}
		if err := slow.parse(); err != nil {
			t.Fatalf("parse(%q): %v", doc, err)
		}
		if len(fast.entries) != len(slow.entries) {
			t.Fatalf("builders disagree on %q: %d vs %d entries", doc, len(fast.entries), len(slow.entries))
		}
		for i := range fast.entries {
			if fast.entries[i] != slow.entries[i] {
				t.Fatalf("builders disagree on %q entry %d: %+v vs %+v",
					doc, i, fast.entries[i], slow.entries[i])
			}
		}
	}
}

// integerSpelling reports whether s is a plain integer: an optional minus
// sign followed only by digits, the spelling the tape tags with tapeFlagInt.
func integerSpelling(s string) bool {
	if strings.HasPrefix(s, "-") {
		s = s[1:]
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// TestNodeIntegerReadEquivalence proves Node.Int64 and Node.Float64 agree
// with strconv on every spelling — value, ok verdict, and negative-zero sign
// — whether the number takes the tagged fast path or the fallback, embedded
// both in arrays and as bare documents short enough to force the scalar
// digit loop.
func TestNodeIntegerReadEquivalence(t *testing.T) {
	spellings := []string{
		"0", "-0", "1", "-1", "7", "42", "-42",
		"12", "123", "1234", "12345", "123456", "1234567", "12345678",
		"123456789", "1234567890", "12345678901", "123456789012",
		"1234567890123", "12345678901234", "123456789012345",
		"1234567890123456", "12345678901234567", "123456789012345678",
		"1234567890123456789", "-1234567890123456789",
		"9223372036854775807", "-9223372036854775807",
		"9223372036854775808", "-9223372036854775808", "-9223372036854775809",
		"9999999999999999999", "-9999999999999999999",
		"18446744073709551615", "18446744073709551616",
		"99999999999999999999", "-99999999999999999999",
		"123456789012345678901234567890",
		"9007199254740993", "9007199254740992", "-9007199254740993",
		"1.5", "-1.5", "1.0", "0.5", "0.0", "-0.0", "10.25",
		"3.141592653589793", "1e2", "1E2", "1e+2", "1e-2", "-1e2", "0e0",
		"1e19", "1e308", "1e309", "-1e309", "5e-324", "1e-400",
		"2.2250738585072011e-308", "1.7976931348623157e308",
	}
	check := func(t *testing.T, node Node, s string) {
		t.Helper()
		if node.Kind() != document.Number {
			t.Fatalf("%q: kind = %v, want Number", s, node.Kind())
		}
		if got, want := node.entry.flags()&tapeFlagInt != 0, integerSpelling(s); got != want {
			t.Fatalf("%q: integer flag = %v, want %v", s, got, want)
		}
		wantInt, err := strconv.ParseInt(s, 10, 64)
		wantIntOK := err == nil
		gotInt, gotIntOK := node.Int64()
		if gotIntOK != wantIntOK || (wantIntOK && gotInt != wantInt) {
			t.Fatalf("%q: Int64 = (%d, %v), want (%d, %v)", s, gotInt, gotIntOK, wantInt, wantIntOK)
		}
		wantFloat, err := strconv.ParseFloat(s, 64)
		wantFloatOK := err == nil
		gotFloat, gotFloatOK := node.Float64()
		if gotFloatOK != wantFloatOK ||
			(wantFloatOK && math.Float64bits(gotFloat) != math.Float64bits(wantFloat)) {
			t.Fatalf("%q: Float64 = (%v, %v), want (%v, %v)", s, gotFloat, gotFloatOK, wantFloat, wantFloatOK)
		}
	}
	for _, s := range spellings {
		// Bare documents keep end small, exercising the short-document
		// scalar loop behind the word kernels.
		src := []byte(s)
		storage := make([]IndexEntry, 4)
		tape, err := BuildIndex(src, storage)
		if err != nil {
			t.Fatalf("BuildIndex(%q): %v", s, err)
		}
		check(t, tape.Root(), s)

		// Array elements sit past the opening bracket, exercising the word
		// kernels once the document is long enough to back their loads.
		src = []byte(`[1,"pad pad pad pad pad",` + s + `]`)
		storage = make([]IndexEntry, len(src))
		tape, err = BuildIndex(src, storage)
		if err != nil {
			t.Fatalf("BuildIndex(%q): %v", src, err)
		}
		element, ok := tape.Root().Index(2)
		if !ok {
			t.Fatalf("%q: missing element", s)
		}
		check(t, element, s)
	}
}
