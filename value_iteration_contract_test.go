package slopjson

import (
	"errors"
	"reflect"
	"testing"

	"github.com/thesyncim/slopjson/document"
)

// ---------------------------------------------------------------------------
// iterators. Order, duplicates, early stop, empty containers,
// whitespace-heavy documents, and Index iterator agreement.
// ---------------------------------------------------------------------------

func TestIterationSemantics(t *testing.T) {
	src := []byte(" { \"b\" : 1 ,\n\t\"a\" : [ true , null ] ,\r\"b\" : \"x\" } ")

	// EachObject must deliver every member, duplicates included, in order.
	var keys []string
	var vals []string
	if err := EachObject(src, func(key string, value RawValue) error {
		keys = append(keys, key)
		vals = append(vals, string(value.Bytes()))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, []string{"b", "a", "b"}) {
		t.Errorf("EachObject keys = %q", keys)
	}
	if !reflect.DeepEqual(vals, []string{"1", "[ true , null ]", `"x"`}) {
		t.Errorf("EachObject values = %q", vals)
	}

	// Value tree retains both duplicates in order.
	value, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	members, ok := value.Object()
	if !ok || len(members) != 3 {
		t.Fatalf("Object() = %v, %v", members, ok)
	}
	for i, wantKey := range []string{"b", "a", "b"} {
		if members[i].Key != wantKey {
			t.Errorf("member %d key = %q, want %q", i, members[i].Key, wantKey)
		}
	}
	if got, _ := value.Get("b"); got.Kind() != document.String {
		t.Errorf("Value.Get(b) kind = %v, want last-wins String", got.Kind())
	}

	// Index ObjectIter agrees.
	root := mustBuildIndex(t, src).Root()
	iter, ok := root.ObjectIter()
	if !ok {
		t.Fatal("ObjectIter")
	}
	var iterKeys []string
	for {
		key, val, ok := iter.Next()
		if !ok {
			break
		}
		kb, _ := key.AppendText(nil)
		iterKeys = append(iterKeys, string(kb))
		if val.Kind() == document.Invalid {
			t.Error("iterator value invalid")
		}
	}
	if !reflect.DeepEqual(iterKeys, keys) {
		t.Errorf("ObjectIter keys = %q, EachObject keys = %q", iterKeys, keys)
	}
	// Early stop: sentinel error must be returned as-is and stop iteration.
	stopErr := errIterationStop
	calls := 0
	err = EachObject(src, func(string, RawValue) error {
		calls++
		if calls == 2 {
			return stopErr
		}
		return nil
	})
	if err != stopErr || calls != 2 {
		t.Errorf("early stop: err = %v, calls = %d", err, calls)
	}

	// Empty containers with whitespace.
	if err := EachArray([]byte(" [ \n ] "), func(int, RawValue) error {
		t.Error("callback on empty array")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := EachObject([]byte(" { \t } "), func(string, RawValue) error {
		t.Error("callback on empty object")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Wrong container kinds.
	if err := EachArray([]byte(`{"a":1}`), nil); err == nil {
		t.Error("EachArray accepted an object")
	}
	if err := EachObject([]byte(`[1]`), nil); err == nil {
		t.Error("EachObject accepted an array")
	}

	// EachArray element raw spans are exact even with whitespace padding.
	var elems []string
	var kinds []document.Kind
	if err := EachArray([]byte(`[ 1 , true , { "x" : "y" } , [ 2 , 3 ] ]`), func(index int, v RawValue) error {
		if index != len(elems) {
			t.Fatalf("EachArray index = %d, want %d", index, len(elems))
		}
		elems = append(elems, string(v.Bytes()))
		kinds = append(kinds, v.Kind())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(elems, []string{"1", "true", `{ "x" : "y" }`, "[ 2 , 3 ]"}) {
		t.Errorf("EachArray elements = %q", elems)
	}
	if !reflect.DeepEqual(kinds, []document.Kind{document.Number, document.Bool, document.Object, document.Array}) {
		t.Errorf("EachArray kinds = %v", kinds)
	}
}

var errIterationStop = errors.New("stop iteration")

// Minimal repro for the empty-object Get checkptr fault: when the empty object
// is the last tape entry and storage is exactly sized, Node.Get computed a
// one-past-the-end *IndexEntry before checking the member count. Under
// -race/checkptr instrumentation this is a fatal "converted pointer straddles
// multiple allocations" crash; without instrumentation it silently violates
// the unsafe.Pointer contract.
func TestEmptyObjectGetAtTapeEnd(t *testing.T) {
	index, err := BuildIndex([]byte(`{}`), make([]IndexEntry, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := index.Root().Get("k"); ok {
		t.Fatal("Get on empty object returned ok")
	}

	// Same shape one level down, reached through a pointer.
	index, err = BuildIndex([]byte(`{"a":{}}`), make([]IndexEntry, 3))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := index.Pointer("/a/x"); ok || err != nil {
		t.Fatalf("Pointer(/a/x) = %v, %v", ok, err)
	}
}
