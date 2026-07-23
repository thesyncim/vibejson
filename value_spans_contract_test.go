package slopjson

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/thesyncim/slopjson/document"
)

// ---------------------------------------------------------------------------
// raw spans. Targets never include surrounding whitespace; root
// scalars work; ScanFirstRaw's documented stop-early behavior on trailing garbage.
// ---------------------------------------------------------------------------

func TestRawSpans(t *testing.T) {
	src := []byte("  { \"a\" : [ 1 , 2 ] , \"s\" : \"x\" }  ")
	raw, ok, err := GetRaw(src, "/a")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if string(raw.Bytes()) != "[ 1 , 2 ]" {
		t.Errorf("target span = %q", raw.Bytes())
	}
	node, ok, err := mustBuildIndex(t, src).Pointer("/a")
	if err != nil || !ok || !bytes.Equal(node.Raw().Bytes(), raw.Bytes()) {
		t.Errorf("index span = %q, raw span = %q", node.Raw().Bytes(), raw.Bytes())
	}

	// Root scalar with whitespace padding.
	rootRaw, ok, err := GetRaw([]byte("  42\n"), "")
	if err != nil || !ok || string(rootRaw.Bytes()) != "42" {
		t.Errorf("root scalar = %q, %v, %v", rootRaw.Bytes(), ok, err)
	}
	if rootRaw.Kind() != document.Number {
		t.Errorf("root scalar kind = %v", rootRaw.Kind())
	}

	// GetRaw validates the tail; ScanFirstRaw documents that it does not.
	garbage := []byte(`{"a":1} trailing`)
	if _, _, err := GetRaw(garbage, "/a"); err == nil {
		t.Error("GetRaw did not validate trailing garbage")
	}
	if raw, ok, err := ScanFirstRaw(garbage, "/a"); err != nil || !ok || string(raw.Bytes()) != "1" {
		t.Errorf("ScanFirstRaw stop-early = %q, %v, %v", raw.Bytes(), ok, err)
	}

	// Nested Get on a RawValue re-anchors pointers relative to the target.
	inner, ok, err := raw.Pointer("/1")
	if err != nil || !ok || string(inner.Bytes()) != "2" {
		t.Errorf("RawValue.Get(/1) = %q, %v, %v", inner.Bytes(), ok, err)
	}
	inner, ok, err = raw.ScanFirstPointerCompiled(MustCompilePointer("/1"))
	if err != nil || !ok || string(inner.Bytes()) != "2" {
		t.Errorf("RawValue.ScanFirstPointerCompiled(/1) = %q, %v, %v", inner.Bytes(), ok, err)
	}
}

// ---------------------------------------------------------------------------
// cross-API semantic agreement on a set of adversarial documents:
// every scalar reachable by pointer must decode identically through raw,
// index, AST, and dynamic Unmarshal+manual-walk paths.
// ---------------------------------------------------------------------------

func TestCrossAPIScalarAgreement(t *testing.T) {
	docs := [][]byte{
		[]byte(`{"a":{"b":[1,"xAy",null,true,1.5e3]},"":[-0.0],"k/s":"v"}`),
		[]byte(` [ {"n":9007199254740993} , {"n":-0} , {"s":"𝄞"} ] `),
		[]byte(`{"dup":1,"dup":{"inner":"last"},"other":[[],{}]}`),
	}
	for _, src := range docs {
		var std any
		if err := json.Unmarshal(src, &std); err != nil {
			t.Fatal(err)
		}
		parsed, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		dynamic, err := decodeAnyUseNumberForTest(src)
		if err != nil {
			t.Fatal(err)
		}
		index := mustBuildIndex(t, src)

		var walk func(pointer string, std any)
		walk = func(pointer string, std any) {
			raw, ok, err := GetRaw(src, pointer)
			if err != nil || !ok {
				t.Fatalf("GetRaw(%q) = %v, %v", pointer, ok, err)
			}
			node, ok, err := index.Pointer(pointer)
			if err != nil || !ok {
				t.Fatalf("Index.Pointer(%q) = %v, %v", pointer, ok, err)
			}
			value, ok, err := parsed.Pointer(pointer)
			if err != nil || !ok {
				t.Fatalf("Value.Pointer(%q) = %v, %v", pointer, ok, err)
			}

			// Semantic agreement with the stdlib value at this pointer.
			var fromRaw any
			if err := json.Unmarshal(raw.Bytes(), &fromRaw); err != nil {
				t.Fatalf("raw target %q at %q: %v", raw.Bytes(), pointer, err)
			}
			if !reflect.DeepEqual(fromRaw, std) {
				t.Errorf("pointer %q: raw %v != stdlib %v", pointer, fromRaw, std)
			}
			if !bytes.Equal(node.Raw().Bytes(), raw.Bytes()) {
				t.Errorf("pointer %q: node raw %q != seeker raw %q", pointer, node.Raw().Bytes(), raw.Bytes())
			}

			switch typed := std.(type) {
			case map[string]any:
				n, ok := node.ObjectLen()
				members, mok := value.Object()
				if !ok || !mok {
					t.Fatalf("pointer %q: object accessors failed", pointer)
				}
				// stdlib map drops duplicates; our count includes them.
				if n < len(typed) || len(members) < len(typed) {
					t.Errorf("pointer %q: member counts %d/%d < stdlib %d", pointer, n, len(members), len(typed))
				}
				for key, sub := range typed {
					walk(pointer+"/"+strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1"), sub)
				}
			case []any:
				n, ok := node.ArrayLen()
				arr, aok := value.Array()
				if !ok || !aok || n != len(typed) || len(arr) != len(typed) {
					t.Fatalf("pointer %q: array lengths %d/%d, stdlib %d", pointer, n, len(arr), len(typed))
				}
				for i, sub := range typed {
					walk(pointer+"/"+strconv.Itoa(i), sub)
				}
			case string:
				got, ok := node.AppendText(nil)
				if !ok || string(got) != typed {
					t.Errorf("pointer %q: node string %q, stdlib %q", pointer, got, typed)
				}
				vt, ok := value.Text()
				if !ok || vt != typed {
					t.Errorf("pointer %q: value string %q, stdlib %q", pointer, vt, typed)
				}
			case float64:
				got, ok := node.Float64()
				if !ok || got != typed {
					t.Errorf("pointer %q: node float %g, stdlib %g", pointer, got, typed)
				}
			case bool:
				got, ok := node.Bool()
				if !ok || got != typed {
					t.Errorf("pointer %q: node bool %v, stdlib %v", pointer, got, typed)
				}
				rawBool, rawOK := raw.Bool()
				if !rawOK || rawBool != typed {
					t.Errorf("pointer %q: raw bool %v, stdlib %v", pointer, rawBool, typed)
				}
			case nil:
				if !node.IsNull() || !raw.IsNull() {
					t.Errorf("pointer %q: null not reported", pointer)
				}
			}
		}
		walk("", std)

		// The UseNumber dynamic tree must round-trip to the stdlib value semantically.
		encoded, err := json.Marshal(dynamic)
		if err != nil {
			t.Fatal(err)
		}
		var back any
		if err := json.Unmarshal(encoded, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, std) {
			t.Errorf("dynamic tree %v != stdlib tree %v", back, std)
		}
	}
}

// ---------------------------------------------------------------------------
// owned Parse results and RawValue.Text allocations must not alias a
// mutable src; zero-copy documents its aliasing.
// ---------------------------------------------------------------------------

func TestOwnedValueSurvivesMutation(t *testing.T) {
	src := []byte(`{"clean":"alpha","escaped":"beéta","num":123.5,"arr":["x","y\ny"]}`)
	owned, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	zero, err := ParseOptions(bytes.Clone(src), Options{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = zero
	for i := range src {
		src[i] = '#'
	}
	for key, want := range map[string]string{"clean": "alpha", "escaped": "beéta"} {
		v, ok := owned.Get(key)
		if !ok {
			t.Fatal(key)
		}
		if got, _ := v.Text(); got != want {
			t.Errorf("owned %s = %q after mutation, want %q", key, got, want)
		}
	}
	num, _ := owned.Get("num")
	if text, ok := num.NumberText(); !ok || text != "123.5" {
		t.Errorf("owned number = %q after mutation", text)
	}
	arr, _ := owned.Get("arr")
	if e, ok := arr.Index(1); !ok {
		t.Fatal("arr index")
	} else if got, _ := e.Text(); got != "y\ny" {
		t.Errorf("owned array elem = %q after mutation", got)
	}
}
